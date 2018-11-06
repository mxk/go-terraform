// Package tfx extends Terraform operations for new use cases.
package tfx

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform/config/module"
	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
)

// Ctx maintains a resource provider registry and implements non-standard
// Terraform operations on state and config files. The zero value is a valid
// context with no registered providers.
type Ctx struct {
	providers map[string]tf.ResourceProviderFactory
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
	c.providers[name] = f
	c.resolver = nil
}

// Schema returns the schema for the specified provider.
func (c *Ctx) Schema(name string) (*schema.Provider, error) {
	if f := c.providers[name]; f != nil {
		p, err := f()
		if err == nil {
			if s, ok := p.(*schema.Provider); ok {
				return s, nil
			}
			err = fmt.Errorf("tfx: provider %q (%T) is not a *schema.Provider",
				name, p)
		}
		return nil, err
	}
	return nil, fmt.Errorf("tfx: provider %q not available", name)
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

// normDiff normalizes a diff by removing empty modules and sorting those that
// remain by path.
func normDiff(d *tf.Diff) *tf.Diff {
	keep := d.Modules[:0]
	for _, m := range d.Modules {
		if !m.Empty() {
			keep = append(keep, m)
		}
	}
	sort.Slice(keep, func(i, j int) bool {
		return lessModulePath(keep[i].Path, keep[j].Path)
	})
	d.Modules = keep
	return d
}

// lessModulePath returns true if module path a should be sorted before path b.
func lessModulePath(a, b []string) bool {
	if ar, br := isRootModule(a), isRootModule(b); ar || br {
		return ar && !br
	}
	for len(a) > 0 && len(b) > 0 {
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

// isRootModule returns true if path represents the root module.
func isRootModule(path []string) bool {
	// TODO: Can the first element be anything other than "root"?
	return len(path) == 1 && path[0] == "root"
}

// stateKeyType returns the first component of a resource state key. This will
// be "data" for data resources.
func stateKeyType(k string) string {
	if i := strings.IndexByte(k, '.'); i > 0 {
		return k[:i]
	}
	return ""
}

// diffScore compares a resource state with a new resource diff and returns a
// match quality score. A non-negative score is the total number of attribute
// matches. A negative score is the number of immutable attribute mismatches,
// indicating that the resource would need to be re-created in order to match.
func diffScore(s *tf.InstanceState, d *tf.InstanceDiff) int {
	var neg, pos int
	for at, ad := range d.Attributes {
		// at may be missing from s.Attributes if it's an optional attribute.
		// TODO: May need schema here to figure out what must be in attributes
		if ad.NewComputed || strings.EqualFold(s.Attributes[at], ad.New) {
			pos++
		} else if ad.RequiresNew {
			neg--
		}
	}
	if neg < 0 {
		return neg
	}
	return pos
}
