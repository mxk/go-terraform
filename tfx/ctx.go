// Package tfx extends Terraform operations for new use cases.
package tfx

import (
	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/config/module"
	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
)

// TODO: Pass provider alias configs via Ctx?

// Ctx implements standard and non-standard Terraform operations using a
// provider registry.
type Ctx struct {
	Meta        tf.ContextMeta
	Parallelism int
	Providers   *ProviderReg
}

// DefaultContext returns a context configured to use default providers.
func DefaultContext() Ctx {
	return Ctx{Providers: &Providers}
}

// Refresh updates the state of all resources in s and returns the new state.
func (c *Ctx) Refresh(s *tf.State) (*tf.State, error) {
	opts := c.opts(module.NewEmptyTree(), s, c.Providers)
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
			if _, s := c.Providers.ResourceSchema(r.Type); s != nil {
				setDefaults(r.Primary.Attributes, s.Schema)
			}
		}
	}
}

// Apply does a plan/apply operation to ensure that state s matches config t and
// returns the new state.
func (c *Ctx) Apply(t *module.Tree, s *tf.State) (*tf.State, error) {
	// TODO: Test whether using schema-only resolver for Plan is really faster
	// for complex providers.
	opts := c.opts(t, s, c.Providers.SchemaResolver())
	tc, err := tf.NewContext(&opts)
	if err != nil {
		return nil, err
	}
	p, err := tc.Plan()
	if err != nil || p.Diff.Empty() {
		return tc.State(), err
	}
	opts.Diff = p.Diff
	opts.ProviderResolver = c.Providers
	tc, err = tf.NewContext(&opts)
	if err != nil {
		return nil, err
	}
	return tc.Apply()
}

// Passthrough does a plan/apply operation with no-op provider CRUD methods and
// returns the new state. The providers are prevented from making any API calls,
// and the resulting state becomes a copy of the input config.
func (c *Ctx) Passthrough(t *module.Tree, s *tf.State) (*tf.State, error) {
	opts := c.opts(t, s, c.Providers.SchemaResolver())
	tc, err := tf.NewContext(&opts)
	if err != nil {
		return nil, err
	}
	if p, err := tc.Plan(); err != nil || p.Diff.Empty() {
		return tc.State(), err
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
	opts := c.opts(nil, s, c.Providers)
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
	opts := c.opts(t, s, c.Providers.SchemaResolver())
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

// Conform returns a transformation that associates root module resource states
// in s with their configurations in t. If strict is true, the transform will
// remove any non-conforming resources.
func (c *Ctx) Conform(t *module.Tree, s *tf.State, strict bool) (StateTransform, error) {
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

	// Remove non-conforming resources
	if strict {
		for _, states := range types {
			for k := range states {
				addr, err := stateKeyToAddress(nil, k)
				if err != nil {
					return nil, err
				}
				st[addr] = ""
			}
		}
	}
	return st, nil
}

// opts returns the options for creating a new Terraform context.
func (c *Ctx) opts(t *module.Tree, s *tf.State, r tf.ResourceProviderResolver) tf.ContextOpts {
	if c.Meta.Env == "" {
		c.Meta.Env = "default"
	}
	return tf.ContextOpts{
		Meta:             &c.Meta,
		Module:           t,
		Parallelism:      c.Parallelism,
		State:            s,
		ProviderResolver: r,
	}
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
