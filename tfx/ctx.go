// Package tfx extends Terraform operations for new use cases.
package tfx

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/config/module"
	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
)

// Ctx maintains a resource provider registry and implements non-standard
// Terraform operations on state and config files. The zero value is a valid
// context with no registered providers.
type Ctx struct {
	providers map[string]tf.ResourceProviderFactory
	schemas   map[string]*schema.Provider
	resolver  tf.ResourceProviderResolver
}

// TODO: Need config maker function to generate minimal configs for aliases

// SetProvider adds a resource provider to the context, replacing any other by
// the same name. If p is nil, the provider is removed.
func (c *Ctx) SetProvider(name string, p tf.ResourceProvider) {
	c.SetProviderFactory(name, tf.ResourceProviderFactoryFixed(p))
}

// SetProviderFactory adds a resource provider factory to the context, replacing
// any other by the same name. If f is nil, the provider is removed.
func (c *Ctx) SetProviderFactory(name string, f tf.ResourceProviderFactory) {
	delete(c.schemas, name)
	c.resolver = nil
	if f == nil {
		delete(c.providers, name)
		return
	}
	if c.providers == nil {
		c.providers = make(map[string]tf.ResourceProviderFactory)
	}
	c.providers[name] = f
}

// Schema returns the schema for the specified provider. It returns nil if the
// provider is not registered or not implemented via schema.Provider. The
// returned value is cached and must only be used for local schema operations.
func (c *Ctx) Schema(provider string) *schema.Provider {
	provider = config.ResourceProviderFullName("", provider)
	if i := strings.IndexByte(provider, '.'); i > 0 {
		provider = provider[:i] // Strip alias
	}
	s, ok := c.schemas[provider]
	if !ok {
		if f := c.providers[provider]; f != nil {
			p, err := f()
			if s, ok = p.(*schema.Provider); ok && err == nil {
				s = DeepCopy(s).(*schema.Provider)
				s.ConfigureFunc = nil
			} else {
				s = nil
			}
		}
		if c.schemas == nil {
			c.schemas = make(map[string]*schema.Provider)
		}
		c.schemas[provider] = s
	}
	return s
}

// ResourceSchema returns the provider and resource schema for the specified
// resource type. It returns nil if the type is unknown or not implemented via
// schema.Provider.
func (c *Ctx) ResourceSchema(typ string) (*schema.Provider, *schema.Resource) {
	if p := c.Schema(config.ResourceProviderFullName(typ, "")); p != nil {
		return p, p.ResourcesMap[typ]
	}
	return nil, nil
}

// Refresh updates the state of all resources in s and returns the new state.
func (c *Ctx) Refresh(s *tf.State) (*tf.State, error) {
	opts := c.opts(module.NewEmptyTree(), s)
	tc, err := tf.NewContext(&opts)
	if err != nil {
		return nil, err
	}
	return tc.Refresh()
}

// SetDefaults sets default values for any missing resource attributes in s.
// This is only needed after refreshing a scanned state.
func (c *Ctx) SetDefaults(s *tf.State) {
	for _, m := range s.Modules {
		for _, r := range m.Resources {
			if _, s := c.ResourceSchema(r.Type); s != nil {
				setDefaults(r.Primary.Attributes, s.Schema)
			}
		}
	}
}

// Apply does a plan/apply operation to ensure that state s matches config t and
// returns the new state.
func (c *Ctx) Apply(t *module.Tree, s *tf.State) (*tf.State, error) {
	opts := c.opts(t, s)
	tc, err := tf.NewContext(&opts)
	if err != nil {
		return nil, err
	}
	if _, err = tc.Plan(); err != nil {
		return nil, err
	}
	return tc.Apply()
}

// Patch applies diff d to state s and returns the new state. Unlike the
// standard apply operation, this one does not require a valid config. It works
// by building and walking a modified apply graph that omits all config
// references, which means that node evaluation may have slightly different
// behavior. For example, resource lifecycle information is only available in
// the config, so create-before-destroy behavior cannot be implemented.
func (c *Ctx) Patch(s *tf.State, d *tf.Diff) (*tf.State, error) {
	opts := c.opts(nil, s)
	opts.Diff = d
	return patch(&opts)
}

// Diff return the changes required to apply configuration t to state s. If s is
// nil, an empty state is assumed.
func (c *Ctx) Diff(t *module.Tree, s *tf.State) (*tf.Diff, error) {
	p, err := c.Plan(t, s)
	if err == nil {
		return p.Diff, nil
	}
	return nil, err
}

// Plan returns a plan to apply configuration t to state s. If s is nil, an
// empty state is assumed.
func (c *Ctx) Plan(t *module.Tree, s *tf.State) (*tf.Plan, error) {
	opts := c.opts(t, s)
	tc, err := tf.NewContext(&opts)
	if err != nil {
		return nil, err
	}
	p, err := tc.Plan()
	if err == nil {
		normDiff(p.Diff)
	}
	return p, err
}

// ResourceForID returns a skeleton resource state for the specified provider
// name, resource type, and resource ID. The returned string is a normalized
// resource state key.
func (c *Ctx) ResourceForID(typ, id string) (string, *tf.ResourceState, error) {
	p, r := c.ResourceSchema(typ)
	if p == nil {
		return "", nil, fmt.Errorf("tfx: unknown provider for type %q", typ)
	}
	if r == nil {
		return "", nil, fmt.Errorf("tfx: unknown resource type %q", typ)
	}
	var meta map[string]interface{}
	if r.SchemaVersion > 0 {
		meta = map[string]interface{}{
			"schema_version": strconv.Itoa(r.SchemaVersion),
		}
	}
	// TODO: Use importers?
	return typ + "." + makeName(id), &tf.ResourceState{
		Type: typ,
		Primary: &tf.InstanceState{
			ID:         id,
			Attributes: map[string]string{"id": id},
			Meta:       meta,
		},
		Provider: tf.ResolveProviderName(
			config.ResourceProviderFullName(typ, ""), nil),
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
		sk, err := tf.ParseResourceStateKey(k)
		if err != nil {
			return nil, err
		}
		if sk.Mode != config.ManagedResourceMode {
			continue // TODO: Figure out how data resources should be handled
		}
		states := types[sk.Type]
		if states == nil {
			states = make(map[string]*tf.ResourceState)
			types[sk.Type] = states
		}
		states[k] = r
	}

	// For each resource in nilDiff, find the best match in types
	st := make(StateTransform)
	for _, m := range nilDiff.Modules {
		for k, d := range m.Resources {
			sk, _ := tf.ParseResourceStateKey(k)
			if sk.Mode != config.ManagedResourceMode {
				continue
			}
			states := types[sk.Type]
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

// provider returns a configured resource provider.
func (c *Ctx) provider(name string) (tf.ResourceProvider, error) {
	f := c.providers[name]
	if f == nil {
		return nil, fmt.Errorf("tfx: unknown provider %q", name)
	}
	p, err := f()
	if err != nil {
		return nil, err
	}
	cfg, _ := config.NewRawConfig(make(map[string]interface{}))
	return p, p.Configure(tf.NewResourceConfig(cfg))
}

// setDefaults sets missing attributes in attrs to their default values.
func setDefaults(attrs map[string]string, s map[string]*schema.Schema) {
	w := schema.MapFieldWriter{Schema: s}
	for k, s := range s {
		// Only set primitive types
		switch s.Type {
		case schema.TypeBool,
			schema.TypeInt,
			schema.TypeFloat,
			schema.TypeString:
		default:
			continue
		}

		// The attribute must be simple, non-computed, and optional
		if _, ok := attrs[k]; ok || s.Default == nil ||
			!s.Optional || s.Computed || s.ForceNew ||
			len(s.ComputedWhen) > 0 || len(s.ConflictsWith) > 0 ||
			s.Deprecated != "" || s.Removed != "" {
			continue
		}

		// Intentionally not using DefaultValue() to avoid any non-deterministic
		// behavior from DefaultFunc.
		w.WriteField([]string{k}, s.Default)
	}
	for k, v := range w.Map() {
		attrs[k] = v
	}
}
