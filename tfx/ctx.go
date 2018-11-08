// Package tfx extends Terraform operations for new use cases.
package tfx

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/config/module"
	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
	"github.com/mitchellh/reflectwalk"
)

// Ctx maintains a resource provider registry and implements non-standard
// Terraform operations on state and config files. The zero value is a valid
// context with no registered providers.
type Ctx struct {
	providers map[string]tf.ResourceProviderFactory
	schemas   map[string]*schema.Provider
	resolver  tf.ResourceProviderResolver
}

// SetProvider adds a resource provider to the context, replacing any other by
// the same name. If p is nil, the provider is removed.
func (c *Ctx) SetProvider(name string, p tf.ResourceProvider) {
	c.SetProviderFactory(name, tf.ResourceProviderFactoryFixed(p))
}

// SetProviderFactory adds a resource provider factory to the context, replacing
// any other by the same name. If f is nil, the provider is removed.
func (c *Ctx) SetProviderFactory(name string, f tf.ResourceProviderFactory) {
	if c.providers == nil {
		c.providers = make(map[string]tf.ResourceProviderFactory)
	}
	delete(c.schemas, name)
	c.providers[name] = f
	c.resolver = nil
}

// Schema returns the schema for the specified provider. It returns nil if the
// provider is not registered or not implemented via schema.Provider. The
// returned value is cached and must only be used for local schema operations.
func (c *Ctx) Schema(provider string) *schema.Provider {
	if s := c.schemas[provider]; s != nil {
		return s
	}
	if f := c.providers[provider]; f != nil {
		p, err := f()
		if s, ok := p.(*schema.Provider); ok && err == nil {
			if c.schemas == nil {
				c.schemas = make(map[string]*schema.Provider)
			}
			c.schemas[provider] = s
			s.ConfigureFunc = nil
			return s
		}
	}
	return nil
}

// ResourceSchema returns the schema for the specified resource type and its
// provider. It returns nil if the type is unknown or not implemented via
// schema.Provider.
func (c *Ctx) ResourceSchema(typ string) (*schema.Provider, *schema.Resource) {
	p := c.Schema(TypeProvider(typ))
	if p != nil {
		return p, p.ResourcesMap[typ]
	}
	return nil, nil
}

// Refresh updates the state of all resources in s.
func (c *Ctx) Refresh(s *tf.State) error {
	opts := c.opts(module.NewEmptyTree(), s)
	tc, err := tf.NewContext(&opts)
	if err != nil {
		return err
	}
	t, err := tc.Refresh()
	if err == nil {
		*s = *t
	}
	return err
}

// Patch applies a diff to an existing state.
func (c *Ctx) Patch(s *tf.State, d *tf.Diff) (*tf.State, error) {
	// TODO: Don't need state transform.
	s, d = s.DeepCopy(), d.DeepCopy()
	st, err := NormStateKeys(s)
	if err != nil {
		return nil, err
	}
	inv := st.Inverse()
	if inv == nil {
		panic("tfx: no inverse for NormStateKeys")
	}
	if err = st.Apply(s); err != nil {
		return nil, err
	}
	if err = st.ApplyToDiff(d); err != nil {
		return nil, err
	}
	providers := make(map[string]*schema.Provider)
	for _, m := range s.Modules {
		md := d.ModuleByPath(m.Path)
		if md == nil {
			continue
		}
		info := tf.InstanceInfo{ModulePath: m.Path}
		for k, r := range m.Resources {
			diff := md.Resources[k]
			if diff == nil {
				continue
			}
			provider := TypeProvider(r.Type)
			p := providers[provider]
			if p == nil {
				if p = c.Schema(provider); p == nil {
					return nil, fmt.Errorf("tfx: no provider for type %q", r.Type)
				}
				p = DeepCopy(p).(*schema.Provider)
				bypassCRUD(p)
				providers[provider] = p
			}
			info.Id = k
			info.Type = r.Type
			r.Primary, err = p.Apply(&info, r.Primary, diff)
			if err != nil {
				return nil, err
			}
			if r.Primary == nil {
				delete(m.Resources, k)
			}
		}
	}
	// TODO: Creation
	return s, inv.Apply(s)
}

// Transition executes plan and apply operations to transition states.
func (c *Ctx) Transition(have, want *tf.State) (*tf.State, error) {
	t, err := c.configFromState(want)
	if err != nil {
		return nil, err
	}
	opts := c.opts(t, have)
	tc, err := tf.NewContext(&opts)
	if err != nil {
		return nil, err
	}
	if _, err = tc.Plan(); err != nil {
		return nil, err
	}
	return tc.Apply()
}

// Diff return the changes required to apply configuration t to state s. If s is
// nil, an empty state is assumed.
func (c *Ctx) Diff(t *module.Tree, s *tf.State) (*tf.Diff, error) {
	opts := c.opts(t, s)
	tc, err := tf.NewContext(&opts)
	if err != nil {
		return nil, err
	}
	p, err := tc.Plan()
	if err != nil {
		return nil, err
	}
	return normDiff(p.Diff), nil
}

// DiffStates returns a diff between states a and b.
func (c *Ctx) DiffStates(a, b *tf.State) (*tf.Diff, error) {
	t, err := c.configFromState(b)
	if err != nil {
		return nil, err
	}
	return c.Diff(t, a)
}

// ResourceForID returns a skeleton resource state for the specified provider
// name, resource type, and resource ID. The returned string is a normalized
// resource state key.
func (c *Ctx) ResourceForID(typ, id string) (string, *tf.ResourceState, error) {
	provider := TypeProvider(typ)
	p := c.Schema(provider)
	if p == nil {
		return "", nil, fmt.Errorf("tfx: unknown provider %q", provider)
	}
	r := p.ResourcesMap[typ]
	if r == nil {
		return "", nil, fmt.Errorf("tfx: unknown resource type %q", typ)
	}
	var meta map[string]interface{}
	if r.SchemaVersion > 0 {
		meta = map[string]interface{}{
			"schema_version": strconv.Itoa(r.SchemaVersion),
		}
	}
	// TODO: Should default values be set here?
	// TODO: Use importers?
	return typ + "." + makeName(id), &tf.ResourceState{
		Type: typ,
		Primary: &tf.InstanceState{
			ID:         id,
			Attributes: map[string]string{"id": id},
			Meta:       meta,
		},
		Provider: tf.ResolveProviderName(provider, nil),
	}, nil
}

// Conform returns a transformation that associates root module resource states
// in s with their configurations in t.
func (c *Ctx) Conform(t *module.Tree, s *tf.State) (StateTransform, error) {
	root := s.RootModule()
	if len(root.Resources) == 0 {
		return nil, nil
	}

	// Create a diff that would be used to apply t from scratch. This generates
	// all resource state keys and interpolates non-computed attribute values.
	nilDiff, err := c.Diff(t, nil)
	if err != nil || nilDiff.Empty() {
		return nil, err
	}

	// Create a type index for all resources in root
	types := make(map[string]map[string]*tf.ResourceState)
	for k, r := range root.Resources {
		if stateKeyType(k) == "data" {
			continue // TODO: Figure out how data resources should be handled
		}
		states := types[r.Type]
		if states == nil {
			states = make(map[string]*tf.ResourceState)
			types[r.Type] = states
		}
		states[k] = r
	}

	// For each resource in nilDiff, find the best match in types
	st := make(StateTransform)
	for _, m := range nilDiff.Modules {
		for k, d := range m.Resources {
			typ := stateKeyType(k)
			states := types[typ]
			bestScore := -1
			var bestKey string
			for sk, s := range states {
				if ds := diffScore(s.Primary, d); bestScore < ds {
					bestScore, bestKey = ds, sk
				}
			}
			if bestScore < 0 {
				continue // TODO: Require at least one attribute match?
			}
			src, err := stateKeyToAddress(nil, bestKey)
			if err != nil {
				return nil, err
			}
			dst, err := stateKeyToAddress(m.Path, k)
			if err != nil {
				return nil, err
			}
			st[src] = dst
			delete(states, bestKey)
		}
	}
	return st, nil
}

// opts returns the options for creating a new Terraform context.
func (c *Ctx) opts(t *module.Tree, s *tf.State) tf.ContextOpts {
	if c.resolver == nil {
		cpy := make(map[string]tf.ResourceProviderFactory, len(c.providers))
		for k, v := range c.providers {
			cpy[k] = v
		}
		c.resolver = tf.ResourceProviderResolverFixed(cpy)
	}
	return tf.ContextOpts{
		Meta:             &tf.ContextMeta{Env: "default"},
		Module:           t,
		State:            s,
		ProviderResolver: c.resolver,
	}
}

// configFromState converts a state into a config. Indexed resources are not
// supported because they don't have 1:1 mapping with the config.
func (c *Ctx) configFromState(s *tf.State) (*module.Tree, error) {
	m := s.RootModule() // TODO: Handle other modules?
	cfg := &config.Config{
		Resources: make([]*config.Resource, 0, len(m.Resources)),
	}
	providers := make(map[string]struct{})
	for k, r := range m.Resources {
		sk, err := tf.ParseResourceStateKey(k)
		if err != nil {
			return nil, err
		}
		if sk.Index != -1 {
			return nil, fmt.Errorf(
				"tfx: cannot create config from indexed state for %q", k)
		}
		provider := TypeProvider(r.Type)
		p := c.Schema(provider)
		if p == nil {
			continue
		}
		rs := p.ResourcesMap[r.Type]
		if rs == nil {
			continue
		}
		providers[provider] = struct{}{}
		count, _ := config.NewRawConfig(map[string]interface{}{"count": "1"})
		count.Key = "count"
		cfg.Resources = append(cfg.Resources, &config.Resource{
			Mode:      sk.Mode,
			Name:      sk.Name,
			Type:      sk.Type,
			RawCount:  count,
			RawConfig: configFromResourceState(rs, r.Primary),
		})
	}
	empty, _ := config.NewRawConfig(make(map[string]interface{}))
	cfg.ProviderConfigs = make([]*config.ProviderConfig, 0, len(providers))
	for p := range providers {
		// TODO: May need to set non-optional attributes
		cfg.ProviderConfigs = append(cfg.ProviderConfigs,
			&config.ProviderConfig{Name: p, RawConfig: empty})
	}
	t := module.NewTree("", cfg)
	if err := t.Load(&module.Storage{Mode: module.GetModeNone}); err != nil {
		return nil, err
	}
	return t, nil
}

// configFromResourceState creates a raw config from an existing state.
func configFromResourceState(r *schema.Resource, s *tf.InstanceState) *config.RawConfig {
	d := r.Data(s)
	m := make(map[string]interface{}, len(r.Schema))
	for k, rs := range r.Schema {
		if !rs.Required && !rs.Optional {
			continue // Computed-only field
		}
		if v, ok := d.GetOk(k); ok { // TODO: Should this use GetOkExists?
			m[k] = makeRaw(v)
		}
	}
	raw, err := config.NewRawConfig(m)
	if err != nil {
		panic(err)
	}
	return raw
}

// makeRaw converts a value from *schema.ResourceData into a representation that
// can be used in *config.RawConfig.
func makeRaw(v interface{}) interface{} {
	switch v := v.(type) {
	case []interface{}:
		for i := range v {
			v[i] = makeRaw(v[i])
		}
	case map[string]interface{}:
		for k := range v {
			v[k] = makeRaw(v[k])
		}
	case *schema.Set:
		l := v.List()
		for i := range l {
			l[i] = makeRaw(l[i])
		}
		return l
	}
	return v
}

// stateKeyType returns the first component of a resource state key. This will
// be "data" for data resources.
func stateKeyType(k string) string {
	if i := strings.IndexByte(k, '.'); i > 0 {
		return k[:i]
	}
	return ""
}

// bypassCRUD replaces all CRUD functions in v with no-ops. It is used to apply
// diffs via a schema.Provider without actually making any API calls.
func bypassCRUD(v interface{}) {
	noop := func(*schema.ResourceData, interface{}) error { return nil }
	reflectwalk.Walk(v, newReplaceWalker(
		schema.CreateFunc(func(r *schema.ResourceData, _ interface{}) error {
			r.SetId("unknown")
			return nil
		}),
		schema.ReadFunc(noop),
		schema.UpdateFunc(noop),
		schema.DeleteFunc(noop),
		schema.ExistsFunc(nil),
	))
}

// replaceWalker is a reflectwalk.PrimitiveWalker that replaces primitives of
// specific types with new values.
type replaceWalker map[reflect.Type]reflect.Value

func newReplaceWalker(v ...interface{}) replaceWalker {
	w := make(replaceWalker, len(v))
	for _, e := range v {
		w[reflect.TypeOf(e)] = reflect.ValueOf(e)
	}
	return w
}

func (w replaceWalker) Primitive(v reflect.Value) error {
	if v.CanSet() {
		if repl, ok := w[v.Type()]; ok {
			v.Set(repl)
		}
	}
	return nil
}
