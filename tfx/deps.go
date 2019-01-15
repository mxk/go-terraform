package tfx

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
)

// DepMap is a resource dependency inference map. Keys are resource types.
// Values are dependency specifications for attributes of that type. These maps
// are usually generated for each provider via depgen.
type DepMap map[string][]DepSpec

// DepSpec specifies that the value of attribute Attr is obtained in HCL by
// interpolating "${SrcType.<name>.SrcAttr}". Resource dependencies are inferred
// by comparing the value(s) of the destination attribute with those of all
// available sources.
type DepSpec struct{ Attr, SrcType, SrcAttr string }

// Deps is the global dependency inference map.
var Deps = make(DepMap)

// Add copies all entries from m to dm.
func (dm DepMap) Add(m DepMap) {
	for k, v := range m {
		if dm[k] != nil {
			panic("tfx: duplicate resource type: " + k)
		}
		dm[k] = v
	}
}

// Infer updates dependencies for all resources in s. This is most commonly used
// for states created via a scan.
func (dm DepMap) Infer(s *tf.State) {
	for _, m := range s.Modules {
		typeMap := make(map[string][]Resource, len(m.Resources))
		for k, r := range m.Resources {
			typeMap[r.Type] = append(typeMap[r.Type], Resource{
				Key:           k,
				ResourceState: r,
			})
		}
		for dstType, rs := range typeMap {
			spec := dm[dstType]
			if len(spec) == 0 {
				continue
			}
			for i := range rs {
				r := &rs[i]
				n := len(r.Dependencies)
				for j := range spec {
					spec[j].infer(r, typeMap)
				}
				if len(r.Dependencies) != n {
					r.Dependencies = unique(r.Dependencies)
				}
			}
		}
	}
}

func (ds *DepSpec) infer(dst *Resource, typeMap map[string][]Resource) {
	srcs := typeMap[ds.SrcType]
	if len(srcs) == 0 {
		return
	}
	vals := getVals(dst, ds.Attr)
	if len(vals) == 0 {
		return
	}
	// TODO: Disallow dependencies between same types? Detect cycles?
	for i := range srcs {
		src := &srcs[i]
		if dst.Key == src.Key {
			continue
		}
		// There should be just one source value, but the destination may have
		// multiple list values matching multiple sources of the same type (e.g.
		// aws_iam_user_group_membership.groups).
		if sv := getVals(src, ds.SrcAttr); len(sv) == 1 {
			for _, dv := range vals {
				if dv == sv[0] {
					dst.Dependencies = append(dst.Dependencies, src.Key)
					break
				}
			}
		} else if len(sv) > 1 {
			panic(fmt.Sprintf("tfx: multiple source values for %s.%s",
				ds.SrcType, ds.SrcAttr))
		}
	}
}

// getVals returns all non-empty values of the specified attribute. The
// attribute may be nested, such as "attr1.attr2". Multiple values may be
// returned if attr refers to any lists or sets.
func getVals(r *Resource, attr string) (vals []string) {
	if v, ok := r.Primary.Attributes[attr]; !ok {
		attr, next := splitAttr(attr)
		getValsHelper(r.Data().Get(attr), r.Type, next, &vals)
	} else if v != "" {
		vals = []string{v}
	}
	return
}

func getValsHelper(v interface{}, typ, next string, vals *[]string) {
	switch v := v.(type) {
	case string:
		if next != "" {
			panic(fmt.Sprintf("tfx: unexpected next attribute for %s: %s",
				typ, next))
		}
		if v != "" {
			*vals = append(*vals, v)
		}
	case []interface{}:
		for _, e := range v {
			getValsHelper(e, typ, next, vals)
		}
	case map[string]interface{}:
		if next == "" {
			for _, e := range v {
				getValsHelper(e, typ, next, vals)
			}
		} else {
			attr, next := splitAttr(next)
			getValsHelper(v[attr], typ, next, vals)
		}
	case *schema.Set:
		if v.Len() > 0 {
			for _, e := range v.List() {
				getValsHelper(e, typ, next, vals)
			}
		}
	case nil:
	default:
		panic(fmt.Sprintf("tfx: unexpected value type for %s: %T", typ, v))
	}
}

func splitAttr(s string) (attr, next string) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func unique(s []string) []string {
	if len(s) < 2 {
		return s
	}
	sort.Strings(s)
	keep := s[:1]
	for _, v := range s[1:] {
		if keep[len(keep)-1] != v {
			keep = append(keep, v)
		}
	}
	return keep
}
