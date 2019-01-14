// Package depgen extracts resource dependency information from Terraform
// provider documentation and test files.
package depgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"text/template"

	"github.com/LuminalHQ/cloudcover/x/tfx"
	"github.com/hashicorp/hil"
	hast "github.com/hashicorp/hil/ast"
	"github.com/hashicorp/terraform/config"
	"github.com/mitchellh/reflectwalk"
	"github.com/pkg/errors"
	md "github.com/russross/blackfriday/v2"
)

func init() { log.SetFlags(0) }

// ModuleDir returns the root module directory where function fn is defined.
func ModuleDir(fn interface{}) string {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		panic("depgen: fn is not a function")
	}
	f := runtime.FuncForPC(v.Pointer())
	path, _ := f.FileLine(f.Entry())
	for strings.IndexByte(filepath.Base(path), '@') < 0 {
		prev := path
		if path = filepath.Dir(path); path == prev {
			panic("depgen: module directory not found")
		}
	}
	return path
}

// Parser extracts interpolated attribute values from HCL examples.
type Parser struct {
	TypeMap map[string]AttrMap
	Sources []string

	root string
	file string
	typ  string
	attr []string
	fset *token.FileSet
	buf  bytes.Buffer
}

// ParseDir recursively parses all supported file types in the specified
// directory. It may be called multiple times for different roots.
func (p *Parser) ParseDir(root string) *Parser {
	p.root = root
	base := filepath.Base(root)
	if mod := strings.Replace(base, "@v", " v", 1); mod != base {
		p.Sources = append(p.Sources, mod)
	} else {
		p.Sources = append(p.Sources, root)
	}
	if err := filepath.Walk(root, p.walkFiles); err != nil {
		panic(err) // Abnormal error that can't be handled or ignored
	}
	return p
}

// Filter calls fn for each attribute in p.TypeMap and removes those for which
// fn returns false.
func (p *Parser) Filter(fn func(*AttrVals) bool) *Parser {
	types := make([]string, 0, len(p.TypeMap))
	for typ := range p.TypeMap {
		types = append(types, typ)
	}
	sort.Strings(types)
	var attrs []string
	for _, typ := range types {
		attrMap := p.TypeMap[typ]
		attrs = attrs[:0]
		for attr := range attrMap {
			attrs = append(attrs, attr)
		}
		sort.Strings(attrs)
		for _, attr := range attrs {
			if !fn(attrMap[attr]) {
				delete(attrMap, attr)
			}
		}
		if len(attrMap) == 0 {
			delete(p.TypeMap, typ)
		}
	}
	return p
}

// Model converts parsed attribute information into a dependency map.
func (p *Parser) Model() *Model {
	depMap := make(tfx.DepMap, len(p.TypeMap))
	for typ, attrMap := range p.TypeMap {
		attrs := make([]string, 0, len(attrMap))
		for attr, vals := range attrMap {
			if len(vals.Simple) == 1 {
				attrs = append(attrs, attr)
			} else {
				log.Printf("Ignoring attr with %d simple values: %v",
					len(vals.Simple), vals)
			}
		}
		if len(attrs) == 0 {
			continue
		}
		sort.Strings(attrs)
		spec := make([]tfx.DepSpec, len(attrs))
		for i, k := range attrs {
			v := attrMap[k]
			spec[i] = tfx.DepSpec{
				Attr:    k,
				SrcType: v.Simple[0].Type,
				SrcAttr: v.Simple[0].Attr,
			}
		}
		depMap[typ] = spec
	}
	_, main, _, _ := runtime.Caller(1)
	dir := filepath.Dir(main)
	return &Model{
		Out:     filepath.Join(dir, "depmap.go"),
		Sources: p.Sources,
		Pkg:     filepath.Base(dir),
		MapVar:  "depMap",
		DepMap:  depMap,
	}
}

func (p *Parser) walkFiles(path string, fi os.FileInfo, err error) error {
	if err != nil || !fi.Mode().IsRegular() {
		return errors.Wrapf(err, "failed to walk %q", path)
	}
	var parse func(src []byte) error
	switch filepath.Ext(path) {
	case ".go":
		if strings.HasSuffix(path, "_test.go") {
			parse = p.parseGo
		}
	case ".md", ".markdown":
		parse = p.parseMarkdown
	case ".tf":
		parse = p.parseHCL
	}
	if parse == nil {
		return nil
	}
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return errors.Wrapf(err, "failed to read %q", p.file)
	}
	if p.file, _ = filepath.Rel(p.root, path); p.file == "" {
		p.file = path
	}
	return parse(b)
}

func (p *Parser) parseGo(b []byte) error {
	p.fset = token.NewFileSet()
	f, err := parser.ParseFile(p.fset, p.file, b, 0)
	if err == nil {
		ast.Walk(goVisitor{p}, f)
	}
	return errors.Wrapf(err, "failed to parse %q", p.file)
}

func (p *Parser) parseMarkdown(b []byte) error {
	block := 0
	n := md.New(md.WithExtensions(md.FencedCode)).Parse(b)
	n.Walk(func(n *md.Node, _ bool) md.WalkStatus {
		if n.Type == md.CodeBlock && string(n.CodeBlockData.Info) == "hcl" {
			if block++; bytes.Contains(n.Literal, []byte("${")) {
				if err := p.parseHCL(n.Literal); err != nil {
					log.Printf("Error parsing HCL in %q (block #%d): %v",
						p.file, block, err)
				}
			}
		}
		return md.GoToNext
	})
	return nil
}

func (p *Parser) parseHCL(b []byte) error {
	c, err := config.LoadJSON(json.RawMessage(b))
	if err != nil {
		return err
	}
	for _, r := range c.Resources {
		if r.Mode == config.ManagedResourceMode {
			p.typ = r.Type
			p.attr = p.attr[:0]
			reflectwalk.Walk(r.RawConfig.Raw, attrWalker{p})
		}
	}
	return nil
}

func (p *Parser) addVal(v *Val) {
	attrMap := p.TypeMap[p.typ]
	if attrMap == nil {
		attrMap = make(AttrMap)
		if p.TypeMap == nil {
			p.TypeMap = make(map[string]AttrMap)
		}
		p.TypeMap[p.typ] = attrMap
	}
	attr := strings.Join(p.attr, ".")
	vals := attrMap[attr]
	if vals == nil {
		vals = &AttrVals{Type: p.typ, Attr: attr}
		attrMap[attr] = vals
	}
	if v.Simple() {
		for _, s := range vals.Simple {
			if s.Type == v.Type && s.Attr == v.Attr {
				return // Ignore duplicates
			}
		}
		vals.Simple = append(vals.Simple, v)
	} else {
		for _, c := range vals.Complex {
			if c.Raw == v.Raw {
				return
			}
		}
		vals.Complex = append(vals.Complex, v)
	}
}

// AttrMap associates attributes of one resource type with their interpolated
// values.
type AttrMap map[string]*AttrVals

// AttrVals contains all discovered values for one attribute. Attr may refer to
// a nested attribute within the type (e.g. "attr1.attr2.attr3"). Simple values
// are those composed of exactly one managed resource interpolation, such as
// "${resource_type.name.attr}". Complex values may include literal text,
// multiple interpolations, function calls, etc. These can normally be ignored
// for the purposes of dependency inference, but sometimes may require
// provider-specific logic to generate the correct DepSpec.
type AttrVals struct {
	Type    string
	Attr    string
	Simple  []*Val
	Complex []*Val
}

// String implements fmt.Stringer.
func (v *AttrVals) String() string {
	vals := make([]string, len(v.Simple)+len(v.Complex))
	for i, s := range v.Simple {
		vals[i] = s.Raw
	}
	for i, c := range v.Complex {
		vals[i+len(v.Simple)] = c.Raw
	}
	return fmt.Sprintf("%s.%s = %q", v.Type, v.Attr, vals)
}

// Val is an attribute value that contains interpolations. Type and Attr are set
// only for simple interpolations.
type Val struct {
	File string
	Raw  string
	Type string
	Attr string
	Root *hast.Output
	Vars []config.InterpolatedVariable
}

// NewVal parses a HashiCorp Interpolation Language (HIL) string and returns a
// new Val if it contains at least one interpolated resource expression.
func NewVal(file, raw string) (*Val, error) {
	if !strings.Contains(raw, "${") {
		return nil, nil
	}
	root, err := hil.Parse(raw)
	if err != nil {
		return nil, err
	}
	n, _ := root.(*hast.Output)
	if n == nil {
		return nil, nil // Literal string (e.g. "$${literal}")
	}
	vars, err := config.DetectVariables(n)
	var src *config.ResourceVariable
	for _, v := range vars {
		r, _ := v.(*config.ResourceVariable)
		if r != nil && r.Mode == config.ManagedResourceMode {
			src = r
			break
		}
	}
	if err != nil || src == nil {
		return nil, err
	}
	v := &Val{File: file, Raw: raw, Root: n, Vars: vars}
	if len(n.Exprs) == 1 {
		if _, ok := n.Exprs[0].(*hast.VariableAccess); ok {
			// Simple value
			v.Type = src.Type
			v.Attr = src.Field
		}
	}
	return v, nil
}

// Simple returns true for values with just one resource interpolation.
func (v *Val) Simple() bool { return v.Type != "" }

// String implements fmt.Stringer.
func (v *Val) String() string { return v.Raw }

// Model is passed to a template to generate Go source code for the dependency
// map.
type Model struct {
	Out     string
	Sources []string
	Pkg     string
	MapVar  string
	DepMap  tfx.DepMap
}

const tpl = `// Code generated by depgen; DO NOT EDIT.
{{- range .Sources}}
// Source: {{.}}
{{- end}}

package {{.Pkg}}

import "{{.DepMapType.PkgPath}}"

var {{.MapVar}} = {{.DepMapType}}{{with .DepMap}}{
{{- range $k, $v := .}}
	"{{$k}}": {
	{{- range $v}}
		{Attr: "{{.Attr}}", SrcType: "{{.SrcType}}", SrcAttr: "{{.SrcAttr}}"},
	{{- end}}
	},
{{- end}}
}{{else}}{}{{end}}
`

// DepMapType returns tfx.DepMap type.
func (m *Model) DepMapType() reflect.Type { return reflect.TypeOf(m.DepMap) }

// Write generates Go source code from the model and writes the output to m.Out.
func (m *Model) Write() {
	t, err := template.New("").Parse(tpl)
	if err == nil {
		var b bytes.Buffer
		if err = t.Execute(&b, m); err == nil {
			if m.Out == "" || m.Out == "-" {
				_, err = b.WriteTo(os.Stdout)
			} else {
				err = ioutil.WriteFile(m.Out, b.Bytes(), 0666)
			}
		}
	}
	if err != nil {
		panic(err)
	}
}

// goVisitor implements ast.Visitor. It calls parseHCL for all raw HCL strings
// found in Go source code.
type goVisitor struct{ *Parser }

func (v goVisitor) Visit(n ast.Node) ast.Visitor {
	if n, _ := n.(*ast.BasicLit); n != nil && n.Kind == token.STRING &&
		n.Value[0] == '`' && strings.Contains(n.Value, "${") &&
		strings.Contains(n.Value, "\nresource \"") {
		v.buf.Reset()
		v.buf.WriteString(n.Value[1 : len(n.Value)-1])
		if err := v.parseHCL(unfmt(v.buf.Bytes())); err != nil {
			log.Printf("Error parsing HCL in %q (line %d): %v",
				v.file, v.fset.Position(n.Pos()).Line, err)
		}
	}
	return v
}

// attrWalker implements reflectwalk interfaces to extract interpolated
// attribute values from RawConfig.
type attrWalker struct{ *Parser }

func (attrWalker) Map(reflect.Value) error          { return nil }
func (attrWalker) Enter(reflectwalk.Location) error { return nil }

func (w attrWalker) MapElem(m, k, v reflect.Value) error {
	w.attr = append(w.attr, k.String())
	return nil
}

func (w attrWalker) Exit(loc reflectwalk.Location) error {
	if loc == reflectwalk.MapValue {
		w.attr = w.attr[:len(w.attr)-1]
	}
	return nil
}

func (w attrWalker) Primitive(v reflect.Value) error {
	if v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if v.Kind() != reflect.String {
		return nil
	}
	val, err := NewVal(w.file, v.String())
	if val != nil {
		w.addVal(val)
	}
	return err
}

// unfmt replaces fmt verbs in b with mock values. This allows parsing HCL
// configs that are normally pre-processed with fmt.Sprintf().
func unfmt(v []byte) []byte {
	start, fill := -1, byte(' ')
	for i, b := range v {
		if start < 0 {
			if b == '%' {
				start = i
			}
			continue
		}
		switch b {
		case ' ', '#', '*', '+', '-', '.', '[', ']':
			continue // Flags and indices (digits are handled in default case)
		case 'b', 'd', 'E', 'e', 'F', 'f', 'G', 'g', 'o', 'p', 't', 'X', 'x':
			fill = '0' // Numeric and boolean values
		case 'c', 'T', 'U':
		case 'q':
			// %q is replaced by a space-filled string
			v[start], v[i] = '"', '"'
			start++
			i--
		case 's', 'v':
			// These are used in too many different contexts, so we just erase
			// the entire line except for a few special cases. A review of lines
			// containing "${" showed that nothing of value is lost for either
			// AWS or Azure providers.
			j := i + bytes.IndexAny(v[i:], "\r\n")
			if j < i {
				j = len(v)
			}
			if tail := v[i:j]; tail[len(tail)-1] == '{' {
				fill = 'x' // Start of a block (%s for ident or string)
			} else if bytes.HasSuffix(tail, []byte("EOF")) {
				fill = '\n' // Heredoc
			} else {
				start = bytes.LastIndexByte(v[:i], '\n') + 1
				i = j - 1
			}
		default:
			if b < '0' || '9' < b {
				start = -1
			}
			continue
		}
		x := v[start : i+1]
		for i := range x {
			x[i] = fill
		}
		start, fill = -1, ' '
	}
	return v
}
