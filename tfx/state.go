package tfx

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/state"
	tf "github.com/hashicorp/terraform/terraform"
	"github.com/mitchellh/copystructure"
)

// NewState returns an initialized empty state.
func NewState() *tf.State {
	// Don't use tf.NewState to avoid logging output
	s := &tf.State{
		Version: tf.StateVersion,
		Lineage: "00000000-0000-0000-0000-000000000000",
	}
	s.AddModule([]string{"root"})
	return s
}

// ReadState reads Terraform state from the specified file ("" or "-" mean
// stdin).
func ReadState(file string) (*tf.State, error) {
	var r io.Reader
	if file == "" || file == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	return tf.ReadState(r)
}

// WriteState writes Terraform state to the specified file ("" or "-" mean
// stdout).
func WriteState(file string, s *tf.State) error {
	if file == "" || file == "-" {
		return tf.WriteState(s, os.Stdout)
	}
	ls := state.LocalState{Path: file}
	return ls.WriteState(s)
}

// AddState performs 'a += b' operation on resources in a. Duplicate resources
// are ignored.
func AddState(a, b *tf.State) *tf.State {
	for _, bm := range b.Modules {
		am := a.ModuleByPath(bm.Path)
		if am == nil {
			a.AddModuleState(DeepCopy(bm).(*tf.ModuleState))
			continue
		}
		for k, r := range bm.Resources {
			if am.Resources[k] == nil {
				am.Resources[k] = DeepCopy(r).(*tf.ResourceState)
			}
		}
	}
	return a
}

// SubState performs 'a -= b' operation on resources in a.
func SubState(a, b *tf.State) *tf.State {
	for _, bm := range b.Modules {
		if am := a.ModuleByPath(bm.Path); am != nil {
			for k := range bm.Resources {
				delete(am.Resources, k)
			}
		}
	}
	return a
}

// ClearDeps clears all resource dependencies.
func ClearDeps(s *tf.State) {
	for _, m := range s.Modules {
		for _, r := range m.Resources {
			r.Dependencies = r.Dependencies[:0]
		}
	}
}

// DeepCopy returns a deep copy of v.
func DeepCopy(v interface{}) interface{} {
	return copystructure.Must(copystructure.Copy(v))
}

var (
	makeNameOnce sync.Once
	normNameRE   *regexp.Regexp
)

// TODO: Unexport MakeName after scan logic is moved from tfstate

// MakeName returns a representation of s that would matches config.NameRegexp.
func MakeName(s string) string {
	if s == "" {
		panic("tfx: empty name")
	}
	makeNameOnce.Do(func() {
		normNameRE = regexp.MustCompile(
			`^[^0-9A-Za-z][^0-9A-Za-z-]*|[^0-9A-Za-z-]+`)
	})
	return normNameRE.ReplaceAllLiteralString(s, "_")
}

// NormStateKeys returns a transformation that normalizes resource state keys
// using provider names and resource IDs.
func NormStateKeys(s *tf.State) (StateTransform, error) {
	st := make(StateTransform)
	for _, m := range s.Modules {
		for k, r := range m.Resources {
			sk, err := tf.ParseResourceStateKey(k)
			if err != nil {
				return nil, err
			}
			norm := MakeName(r.Provider + "_" + r.Primary.ID)
			if sk.Mode != config.ManagedResourceMode || sk.Name == norm {
				continue
			}
			sk.Name = norm
			src, err := stateKeyToAddress(m.Path, k)
			if err != nil {
				return nil, err
			}
			dst, err := stateKeyToAddress(m.Path, sk.String())
			if err != nil {
				return nil, err
			}
			st[src] = dst
		}
	}
	if len(st) == 0 {
		st = nil
	}
	return st, nil
}

// StateTransform defines state resource address transformations. It can change
// resource keys, move resources between modules, and remove resources.
// Dependencies are updated as needed as long as they stay within the same
// module. Keys and values are Terraform resource addresses. Resource types are
// not validated. An empty value removes the resource.
type StateTransform map[string]string

// Apply updates resource state keys according to the transformation map. The
// state is not modified if an error is returned. Missing resources are silently
// ignored. Address collisions are resolved in favor of the transformation, so
// the map {A: B} will replace an existing resource B with A. Without an
// explicit {B: ""} entry, resources that depended on B will depend on A after
// such transformation.
func (st StateTransform) Apply(s *tf.State) error {
	if len(st) == 0 {
		return nil
	}
	type node struct {
		addr string
		key  string
		repl *node
		mod  *tf.ModuleState
		res  *tf.ResourceState
		deps []*node
	}

	// Step 1: Add resources from all modules to stateMap, indexed by address
	stateMap := make(map[string]*node, len(s.RootModule().Resources))
	for _, m := range s.Modules {
		if len(m.Resources) == 0 {
			continue
		}

		// Add a node for each resource to stateMap and moduleMap, the latter
		// indexed by state key to resolve dependencies.
		moduleMap := make(map[string]*node, len(m.Resources))
		nodePool := make([]node, len(m.Resources))
		totalDeps := 0
		for k, r := range m.Resources {
			addr, err := stateKeyToAddress(m.Path, k)
			if err != nil {
				return err
			}
			n := &nodePool[0]
			nodePool = nodePool[1:]
			n.addr = addr
			n.key = k
			n.mod = m
			n.res = r
			totalDeps += len(r.Dependencies)
			if stateMap[n.addr] != nil {
				// Sanity check, should never happen
				panic("tfx: address collision: " + n.addr)
			}
			stateMap[n.addr] = n
			moduleMap[n.key] = n
		}

		// Resolve dependencies
		if totalDeps == 0 {
			continue
		}
		linkPool := make([]*node, totalDeps)
		for _, n := range moduleMap {
			if deps := n.res.Dependencies; len(deps) > 0 {
				n.deps = linkPool[:len(deps):len(deps)]
				linkPool = linkPool[len(deps):]
				for i := range deps {
					n.deps[i] = moduleMap[deps[i]] // May be nil
				}
			}
		}
	}

	// Step 2: Apply transformations and store results in transMap
	const rootPrefix = "module.root."
	transMap := make(map[string]*node, len(stateMap))
	for _, n := range stateMap {
		addr, ok := st[n.addr]
		if !ok && strings.HasPrefix(n.addr, rootPrefix) {
			// n.addr is normalized, but st keys and values may not be
			addr, ok = st[n.addr[len(rootPrefix):]]
		}

		// An unmapped resource is kept or replaced
		if !ok {
			if repl := transMap[n.addr]; repl == nil {
				transMap[n.addr] = n
			} else {
				n.repl = repl
				n.mod = nil
			}
			continue
		}

		// A resource mapped to an empty address is removed
		if addr == "" {
			n.mod = nil
			continue
		}
		if !strings.HasPrefix(addr, "module.") {
			addr = rootPrefix + addr
		}

		// A mapped resource may replace an unmapped one
		if u := transMap[addr]; u != nil {
			if u.key == "" {
				// st maps multiple resources to the same address
				return fmt.Errorf("tfx: address collision for %q", addr)
			}
			u.repl = n
			u.mod = nil
		}

		// The key must be updated for the new address
		n.addr = addr
		n.key = ""
		transMap[n.addr] = n
	}

	// Step 3: Update keys, modules, and dependency links
	var tmpState tf.State
	for _, n := range transMap {
		if n.key == "" {
			path, key, err := addressToStateKey(n.addr)
			if err != nil {
				return err
			}
			n.key = key
			n.mod = s.ModuleByPath(path)
			if n.mod == nil {
				// Don't modify the original state just yet
				n.mod = tmpState.AddModule(path)
			}
		}
		for i, d := range n.deps {
			if d != nil && d.repl != nil {
				// Possible cycle if n replaced its own dependency
				n.deps[i] = d.repl
			}
		}
	}

	// Step 4: Clear out original state and add new modules
	for _, m := range s.Modules {
		for k := range m.Resources {
			delete(m.Resources, k)
		}
	}
	for _, m := range tmpState.Modules {
		s.AddModuleState(m)
	}

	// Step 5: Add transformed resources and update dependencies
	depSet := make(map[string]struct{})
	for _, n := range transMap {
		if n.mod.Resources[n.key] != nil {
			// Shouldn't happen since keys are derived from addresses
			panic("tfx: resource state key collision: " + n.key)
		}
		n.mod.Resources[n.key] = n.res
		for i, d := range n.deps {
			if d == nil {
				depSet[n.res.Dependencies[i]] = struct{}{}
			} else if d.mod == n.mod {
				depSet[d.key] = struct{}{}
			}
		}
		deps := n.res.Dependencies[:0]
		for dep := range depSet {
			if delete(depSet, dep); dep != "" && dep != n.key {
				deps = append(deps, dep)
			}
		}
		sort.Strings(deps)
		n.res.Dependencies = deps
	}
	return nil
}

// stateKeyToAddress converts a resource state key into a normalized address.
func stateKeyToAddress(path []string, key string) (string, error) {
	k, err := tf.ParseResourceStateKey(key)
	if err != nil {
		return "", err
	}
	if len(path) == 0 {
		path = tf.RootModulePath
	}
	addr := tf.ResourceAddress{
		Path:  path,
		Index: k.Index,
		Name:  k.Name,
		Type:  k.Type,
		Mode:  k.Mode,
	}
	return addr.String(), nil
}

// addressToStateKey converts a resource address into a state key.
func addressToStateKey(addr string) (path []string, key string, err error) {
	k, err := tf.ParseResourceAddress(addr)
	if err != nil {
		return
	}
	if k.Type == "" || k.Name == "" {
		err = fmt.Errorf("tfx: incomplete resource address %q", addr)
		return
	}
	if len(k.Path) == 0 {
		k.Path = tf.RootModulePath
	}
	sk := tf.ResourceStateKey{
		Name:  k.Name,
		Type:  k.Type,
		Mode:  k.Mode,
		Index: k.Index,
	}
	return k.Path, sk.String(), nil
}
