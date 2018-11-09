package tfx

import (
	"math/rand"
	"sort"

	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
)

// MutateFunc is a function that can modify resources.
type MutateFunc func(*MutateState)

// MutateCfg determines the behavior of the Mutate operation.
type MutateCfg struct {
	Seed  int64
	Limit int
	Funcs []MutateFunc
}

// MutateState contains the state of the current resource as well as the rest
type MutateState struct {
	*schema.ResourceData

	Rand   *rand.Rand
	Diff   *tf.ModuleDiff
	Module *tf.ModuleState
	Type   string
	Key    string
	Schema map[string]*schema.Schema
}

func (c *Ctx) Mutate(s *tf.State, cfg *MutateCfg) (*tf.Diff, error) {
	root := s.RootModule()
	ms := MutateState{
		Rand: rand.New(rand.NewSource(cfg.Seed)),
		Diff: &tf.ModuleDiff{
			Path:      root.Path,
			Resources: make(map[string]*tf.InstanceDiff),
		},
		Module: root,
	}
	keys := make([]string, 0, len(ms.Module.Resources))
	for k := range root.Resources {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ms.Rand.Shuffle(len(keys), func(i, j int) {
		keys[i], keys[j] = keys[j], keys[i]
	})
	info := tf.InstanceInfo{ModulePath: root.Path}
	var changes int
	for _, k := range keys {
		if cfg.Limit > 0 && changes >= cfg.Limit {
			break
		}
		curState := root.Resources[k]
		p, r := c.ResourceSchema(curState.Type)
		if r == nil {
			continue
		}
		ms.ResourceData = r.Data(curState.Primary)
		ms.Type = curState.Type
		ms.Key = k
		ms.Schema = r.Schema
		info.Id = k
		info.Type = ms.Type
		for _, fn := range cfg.Funcs {
			if fn(&ms); ms.Id() == "" {
				ms.Diff.Resources[k] = &tf.InstanceDiff{Destroy: true}
				changes++
				break
			}
			diff, err := p.Diff(&info, curState.Primary,
				tf.NewResourceConfig(configFromResourceState(r, ms.State())))
			if err != nil {
				return nil, err
			}
			if !diff.Empty() {
				ms.Diff.Resources[k] = diff
				changes++
				break
			}
		}
	}
	d := new(tf.Diff)
	if !ms.Diff.Empty() {
		d.Modules = append(d.Modules, ms.Diff)
	}
	return d, nil
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
