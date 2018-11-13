package tfx

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strings"

	tf "github.com/hashicorp/terraform/terraform"
)

// ReadPlanFile reads Terraform plan from the specified file.
func ReadPlanFile(file string) (*tf.Plan, error) {
	r, err := open(file)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return tf.ReadPlan(r)
}

// ReadDiffFile reads Terraform diff from the specified file. It supports both
// JSON-encoded diffs and plan files.
func ReadDiffFile(file string) (*tf.Diff, error) {
	r, err := open(file)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return ReadDiff(r)
}

// ReadDiff reads Terraform diff from r. It supports both JSON-encoded diffs and
// plan files.
func ReadDiff(r io.Reader) (*tf.Diff, error) {
	const magic = "tfplan"
	b := bufio.NewReader(r)
	if v, err := b.Peek(len(magic)); err == nil && string(v) == magic {
		p, err := tf.ReadPlan(b)
		if err != nil {
			return nil, err
		}
		return p.Diff, nil
	}
	d := new(tf.Diff)
	if err := json.NewDecoder(b).Decode(d); err != nil {
		return nil, err
	}
	return d, nil
}

// WriteDiffFile writes diff d to file in JSON format.
func WriteDiffFile(file string, d *tf.Diff) error {
	if isStdio(file) {
		return WriteDiff(os.Stdout, d)
	}
	var b bytes.Buffer
	if err := WriteDiff(&b, d); err != nil {
		return err
	}
	return ioutil.WriteFile(file, b.Bytes(), 0666)
}

// WriteDiff writes diff d to w in JSON format.
func WriteDiff(w io.Writer, d *tf.Diff) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return err
	}

	// TODO: Try to avoid this encode/decode/encode logic
	var v interface{}
	if err := json.Unmarshal(b.Bytes(), &v); err != nil {
		return err
	}
	var minify func(interface{}) interface{}
	minify = func(v interface{}) interface{} {
		switch v := v.(type) {
		case bool:
			if !v {
				return nil
			}
		case string:
			if v == "" {
				return nil
			}
		case []interface{}:
			keep := v[:0]
			for _, e := range v {
				if e = minify(e); e != nil {
					keep = append(keep, e)
				}
			}
			if v = keep; len(keep) == 0 {
				return nil
			}
		case map[string]interface{}:
			for k, e := range v {
				if e = minify(e); e != nil {
					v[k] = e
				} else {
					delete(v, k)
				}
			}
			if len(v) == 0 {
				return nil
			}
		case nil:
		default:
			// float64 is omitted since it's not currently used by Diff and may
			// require special handling.
			panic(fmt.Sprintf("tfx: unsupported type %T", v))
		}
		return v
	}
	enc = json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "\t")
	return enc.Encode(minify(v))
}

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

const stdinLimit = 64 * 1024 * 1024

// open opens the specified file for reading ("" or "-" mean stdin).
func open(file string) (io.ReadCloser, error) {
	if isStdio(file) {
		return ioutil.NopCloser(io.LimitReader(os.Stdin, stdinLimit)), nil
	}
	return os.Open(file)
}

// isStdio returns true if file represents stdin or stdout.
func isStdio(file string) bool {
	return file == "" || file == "-"
}
