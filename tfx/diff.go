package tfx

import (
	"fmt"
	"sort"
	"strings"

	tf "github.com/hashicorp/terraform/terraform"
)

// diffType defines sort order and labels for diff explanation.
var diffType = map[tf.DiffChangeType]struct {
	order int
	label string
}{
	tf.DiffCreate:  {1, "MISSING RESOURCE"},
	tf.DiffDestroy: {2, "EXTRA RESOURCE"},
	tf.DiffUpdate:  {3, "ATTRIBUTE MISMATCH"},
}

// ExplainDiff returns a description of inconsistencies between actual state and
// desired config.
func ExplainDiff(d *tf.Diff) string {
	type resDiff struct {
		*tf.InstanceDiff
		name string
		typ  tf.DiffChangeType
	}
	var diffs []resDiff
	for _, m := range d.Modules {
		if len(diffs) == 0 && len(m.Resources) > 0 {
			diffs = make([]resDiff, 0, len(m.Resources))
		}
		for name, d := range m.Resources {
			switch typ := d.ChangeType(); typ {
			case tf.DiffDestroyCreate:
				typ = tf.DiffUpdate
				fallthrough
			case tf.DiffCreate, tf.DiffDestroy, tf.DiffUpdate:
				diffs = append(diffs, resDiff{d, name, typ})
			}
		}
	}
	sort.Slice(diffs, func(i, j int) bool {
		io, jo := diffType[diffs[i].typ].order, diffType[diffs[j].typ].order
		return io < jo || (io == jo && diffs[i].name < diffs[j].name)
	})
	var b strings.Builder
	var keys []string
	typ := tf.DiffInvalid
	for i := range diffs {
		d := &diffs[i]
		if d.typ != typ {
			if typ != tf.DiffInvalid {
				b.WriteByte('\n')
			}
			b.WriteString(diffType[d.typ].label)
			b.WriteString(":\n")
			typ = d.typ
		} else if typ == tf.DiffUpdate {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(d.name)
		b.WriteString("\n")
		if typ != tf.DiffUpdate {
			continue
		}
		var keyLen int
		keys = keys[:0]
		for key, attr := range d.Attributes {
			if attr.New == attr.Old || (attr.NewComputed && attr.Old != "") {
				continue
			}
			if keys = append(keys, key); keyLen < len(key) {
				keyLen = len(key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			attr := d.Attributes[key]
			have := attr.Old
			want := attr.New
			if attr.NewComputed {
				want = "<computed>"
			}
			if attr.Sensitive {
				have = "<sensitive>"
				want = "<sensitive>, value mismatch"
			}
			fmt.Fprintf(&b, "  %-*s = %q (expected: %q)\n",
				keyLen, key, have, want)
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}
