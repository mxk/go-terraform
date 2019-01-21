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

	"github.com/hashicorp/hil"
	hast "github.com/hashicorp/hil/ast"
	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
	"github.com/mitchellh/reflectwalk"
	"github.com/mxk/cloudcover/x/gomod"
	"github.com/mxk/cloudcover/x/tfx"
	"github.com/pkg/errors"
	md "github.com/russross/blackfriday/v2"
)

// TODO: Log messages should be grouped by type and attribute. Parser should
// probably maintain this log instead of everything being written to stderr
// immediately.

func init() { log.SetFlags(0) }

// Parser extracts interpolated attribute values from HCL examples.
type Parser struct {
	Provider *schema.Provider
	Sources  []string
	TypeMap  map[string]AttrMap

	root string
	file string
	typ  string
	attr []string
	fset *token.FileSet
	buf  bytes.Buffer

	typPrefix string
	schema    map[string]AttrSchema
}

// Parse calls ParseDir on the module root directory of the specified provider.
func (p *Parser) Parse(fn func() tf.ResourceProvider) *Parser {
	if p.Provider, _ = fn().(*schema.Provider); p.Provider != nil {
		p.Provider.ConfigureFunc = nil
		for typ := range p.Provider.ResourcesMap {
			if i := strings.IndexByte(typ, '_'); i > 0 {
				p.typPrefix = typ[:i+1]
				break
			}
		}
	}
	return p.ParseDir(gomod.Root(fn).Path())
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

// idHier is an AttrSchema hierarchy for the common "id" attribute.
var idHier = []*schema.Schema{{
	Type:     schema.TypeString,
	Computed: true,
}}

// Schema returns the schema of the specified resource attribute ("type.name").
func (p *Parser) Schema(typ, attr string) (s AttrSchema) {
	k := typ
	if attr != "" {
		k += "." + attr
	}
	s, ok := p.schema[k]
	if ok || p.Provider == nil || typ == "" {
		return
	}
	s.Resource = p.Provider.ResourcesMap[typ]
	if s.Resource != nil && attr != "" {
		if attr == "id" {
			s.Schema = idHier[0]
			s.Hier = idHier
		} else {
			attr, next := splitAttr(attr)
			if attrSchema(s.Resource.Schema[attr], next, &s.Hier) {
				s.Schema = s.Hier[len(s.Hier)-1]
			}
		}
	}
	if p.schema == nil {
		p.schema = make(map[string]AttrSchema)
	}
	p.schema[k] = s
	return
}

// Apply removes or keeps attributes in p.TypeMap by looking up rules in a map.
// Keys may be "<type>.<attr>", "<type>", or ".<attr>", with lookups performed
// in that order. First match wins.
func (p *Parser) Apply(rules map[string]bool) *Parser {
	for typ, attrMap := range p.TypeMap {
		for name, t := range attrMap {
			keep, ok := rules[t.Key]
			if !ok {
				keep, ok = rules[typ]
				if !ok {
					keep, ok = rules[t.Key[strings.IndexByte(t.Key, '.'):]]
					if !ok {
						continue
					}
				}
			}
			if keep {
				t.Keep()
			} else {
				delete(attrMap, name)
			}
		}
		if len(attrMap) == 0 {
			delete(p.TypeMap, typ)
		}
	}
	return p
}

// Call calls fn for each attribute in p.TypeMap and removes those for which fn
// returns false.
func (p *Parser) Call(fn func(*Attr) bool) *Parser {
	for _, typ := range p.sortedTypes() {
		attrMap := p.TypeMap[typ]
		for _, name := range attrMap.sortedNames() {
			// fn is expected to call Keep explicitly if that is the right thing
			// to do.
			if !fn(attrMap[name]) {
				delete(attrMap, name)
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
	for _, typ := range p.sortedTypes() {
		attrMap := p.TypeMap[typ]
		names := attrMap.sortedNames()
		spec := make([]tfx.DepSpec, 0, len(names))
		for _, name := range names {
			t := attrMap[name]
			if p.Provider != nil && t.Schema == nil {
				log.Printf("Invalid attribute: %v", t)
				continue
			}
			if skip := t.Explain(); skip != "" {
				log.Println(skip)
				continue
			}
			spec = append(spec, tfx.DepSpec{
				Attr:    name,
				SrcType: t.Simple[0].Type,
				SrcAttr: t.Simple[0].Attr,
			})
		}
		if len(spec) > 0 {
			depMap[typ] = spec
		}
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
		if r.Mode == config.ManagedResourceMode &&
			strings.HasPrefix(r.Type, p.typPrefix) {
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
	name := strings.Join(p.attr, ".")
	t := attrMap[name]
	if t == nil {
		t = &Attr{
			AttrSchema: p.Schema(p.typ, name),
			Key:        p.typ + "." + name,
			Type:       p.typ,
			Name:       name,
		}
		attrMap[name] = t
	}
	if v.IsSimple() {
		for _, s := range t.Simple {
			if s.Type == v.Type && s.Attr == v.Attr {
				return // Ignore duplicates
			}
		}
		v.AttrSchema = p.Schema(v.Type, v.Attr)
		t.Simple = append(t.Simple, v)
	} else {
		for _, c := range t.Complex {
			if c.Raw == v.Raw {
				return
			}
		}
		t.Complex = append(t.Complex, v)
	}
}

func (p *Parser) sortedTypes() []string {
	v := make([]string, 0, len(p.TypeMap))
	for typ := range p.TypeMap {
		v = append(v, typ)
	}
	sort.Strings(v)
	return v
}

// AttrMap associates attributes of one resource type with their interpolated
// values.
type AttrMap map[string]*Attr

func (m AttrMap) sortedNames() []string {
	v := make([]string, 0, len(m))
	for name := range m {
		v = append(v, name)
	}
	sort.Strings(v)
	return v
}

// AttrSchema describes the schema of a resource attribute. Hier will contain
// multiple schemas for nested attributes.
type AttrSchema struct {
	Resource *schema.Resource
	Schema   *schema.Schema
	Hier     []*schema.Schema
}

// IsScalar returns true if s refers to exactly one value. It returns false if
// the attribute is part of a list or set, or if it refers to a map without an
// explicit key.
func (s *AttrSchema) IsScalar() bool {
	for i, h := range s.Hier {
		switch h.Type {
		case schema.TypeList, schema.TypeSet:
			return false
		case schema.TypeMap:
			if i+1 == len(s.Hier) {
				return false
			}
		}
	}
	return len(s.Hier) > 0
}

// IsString returns true if s refers to a string attribute.
func (s *AttrSchema) IsString() bool {
	return s.Schema != nil && s.Schema.Type == schema.TypeString
}

// Attr contains all discovered values for one attribute. Attr may refer to a
// nested attribute within the type (e.g. "attr1.attr2.attr3"). Simple values
// are those composed of exactly one managed resource interpolation, such as
// "${resource_type.name.attr}". Complex values may include literal text,
// multiple interpolations, function calls, etc. These can normally be ignored
// for the purposes of dependency inference, but sometimes may require
// provider-specific logic to generate the correct DepSpec.
type Attr struct {
	AttrSchema

	Key     string
	Type    string
	Name    string
	Simple  []*Val
	Complex []*Val
}

// Keep hides all but the first simple value from view, allowing the attribute
// to be included in a DepMap if there are no other problems.
func (t *Attr) Keep() {
	if len(t.Simple) > 0 {
		t.Simple = t.Simple[:1]
	}
	t.Complex = t.Complex[:0]
}

// Explain returns a string explaining why this attribute should not be included
// in a DepMap.
func (t *Attr) Explain() string {
	if len(t.Simple) != 1 {
		return fmt.Sprintf("Attribute with %d simple values: %v",
			len(t.Simple), t)
	}
	if len(t.Complex) > 0 {
		return fmt.Sprintf("Attribute with %d complex value(s): %v",
			len(t.Complex), t)
	}
	if v := t.Simple[0]; t.Schema != nil {
		if !t.IsString() {
			return fmt.Sprintf("Non-string attribute: %v", t)
		}
		if v.Schema == nil {
			return fmt.Sprintf("Value without schema: %v", t)
		}
		if !v.IsString() {
			return fmt.Sprintf("Non-string value: %v", t)
		}
		if !v.IsScalar() {
			return fmt.Sprintf("Non-scalar value: %v", t)
		}
	} else if v.Schema != nil {
		return fmt.Sprintf("Attribute without schema: %v", t)
	}
	return ""
}

// String implements fmt.Stringer.
func (t *Attr) String() string {
	vals := make([]string, len(t.Simple)+len(t.Complex))
	for i, v := range t.Simple {
		vals[i] = v.Raw
	}
	for i, v := range t.Complex {
		vals[i+len(t.Simple)] = v.Raw
	}
	return fmt.Sprintf("%s.%s = %q", t.Type, t.Name, vals)
}

// Val is an attribute value that contains interpolations. AttrSchema, Type, and
// Attr are set only for simple interpolations.
type Val struct {
	AttrSchema

	File string
	Raw  string
	Type string
	Attr string
	Root *hast.Output
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

	// Find all VariableAccess nodes, leaving just one on the stack for simple
	// expressions.
	var allVars vaStack
	s := make(vaStack, 0, 8)
	n.Accept(func(n hast.Node) hast.Node {
		switch n := n.(type) {
		case *hast.Arithmetic:
			s.pop(len(n.Exprs))
			s.push(nil)
		case *hast.Call:
			if v := s.pop(len(n.Args)); n.Func == "element" {
				s.push(v[0])
			} else {
				s.push(nil)
			}
		case *hast.Conditional:
			s.pop(3)
			s.push(nil)
		case *hast.Index:
			s.push(s.pop(2)[0])
		case *hast.LiteralNode:
			s.push(nil)
		case *hast.Output:
		case *hast.VariableAccess:
			s.push(n)
			allVars.push(n)
		default:
			panic(fmt.Sprintf("depgen: unsupported node type: %T", n))
		}
		return n
	})

	// Parse all VariableAccess nodes. At least one resource expression is
	// needed to return a value.
	var v *Val
	for _, va := range allVars {
		interp, err := config.NewInterpolatedVariable(va.Name)
		if err != nil {
			return nil, err
		}
		r, _ := interp.(*config.ResourceVariable)
		if r != nil && r.Mode == config.ManagedResourceMode {
			if v == nil {
				v = &Val{File: file, Raw: raw, Root: n}
			}
			if len(s) == 1 && s[0] == va {
				// Simple value
				v.Type = r.Type
				v.Attr = r.Field
			}
			// Keep going to catch any errors
		}
	}
	return v, nil
}

// IsSimple returns true for values with just one resource interpolation.
func (v *Val) IsSimple() bool { return v.Type != "" }

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

// TODO: Verify that this handles *schema.Set correctly

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

// vaStack is a stack used by NewVal to evaluate AST nodes.
type vaStack []*hast.VariableAccess

func (s *vaStack) push(v *hast.VariableAccess) {
	*s = append(*s, v)
}

func (s *vaStack) pop(n int) (v []*hast.VariableAccess) {
	i := len(*s) - n
	*s, v = (*s)[:i], (*s)[i:]
	return v
}

var stringSchema = schema.Schema{Type: schema.TypeString}

// attrSchema recursively searches schema for the specified attribute. It
// returns true if the attribute is found.
func attrSchema(s *schema.Schema, next string, hier *[]*schema.Schema) bool {
	if s == nil {
		return false
	}
	switch *hier = append(*hier, s); s.Type {
	case schema.TypeList, schema.TypeSet:
		switch e := s.Elem.(type) {
		case *schema.Schema:
			return attrSchema(e, next, hier)
		case *schema.Resource:
			if next == "" {
				return true
			}
			attr, next := splitAttr(next)
			return attrSchema(e.Schema[attr], next, hier)
		default:
			panic(fmt.Sprintf("depgen: unexpected elem type: %T", e))
		}
	case schema.TypeMap:
		if next == "" {
			return true
		}
		// In theory, a map can't contain complex values per docs and this:
		// https://github.com/hashicorp/terraform/issues/6215
		// In practice, there is aws_cognito_identity_pool_roles_attachment,
		// azurerm_log_analytics_workspace_linked_service, and others. In this
		// case, key refers to an attribute in the child resource.
		attr, next := splitAttr(next)
		switch e := s.Elem.(type) {
		case *schema.Schema:
			return attrSchema(e, next, hier)
		case *schema.Resource:
			return attrSchema(e.Schema[attr], next, hier)
		case nil:
			return attrSchema(&stringSchema, next, hier)
		default:
			panic(fmt.Sprintf("depgen: unexpected elem type: %T", e))
		}
	default:
		return next == ""
	}
}

// splitAttr splits s at the first period.
func splitAttr(s string) (attr, next string) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
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
