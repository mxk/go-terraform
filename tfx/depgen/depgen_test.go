package depgen

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform/builtin/providers/test"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/mxk/go-gomod"
	"github.com/mxk/go-terraform/tfx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewVal(t *testing.T) {
	tests := []*struct {
		have string
		want *Val
	}{
		{},
		{"abc", nil},
		{"$${literal}", nil},
		{"${data.resource_type.name.attr}", nil},
		{"${resource_type.name.attr}", &Val{Type: "resource_type", Attr: "attr"}},
		{"${element(resource_type.name.attr[0], count.index)}", &Val{Type: "resource_type", Attr: "attr"}},
		{"complex${resource_type.name.attr}", &Val{}},
	}
	for _, tc := range tests {
		v, err := NewVal("", tc.have)
		require.NoError(t, err, "%+v", tc)
		if tc.want == nil {
			assert.Nil(t, v, "%+v", tc)
		} else if assert.NotNil(t, v, "%+v", tc) {
			tc.want.Raw = tc.have
			v.Root = nil
			assert.Equal(t, tc.want, v, "%+v", tc)
		}
	}
	_, err := NewVal("", "${parse.error")
	require.Error(t, err)
}

func TestParser(t *testing.T) {
	dir := filepath.Dir(gomod.File(TestParser))
	want := &Model{
		Out:     filepath.Join(dir, "depmap.go"),
		Sources: []string{dir},
		Pkg:     filepath.Base(dir),
		MapVar:  "depMap",
		DepMap: tfx.DepMap{
			"aws_iam_user_policy_attachment": {
				{Attr: "policy_arn", SrcType: "aws_iam_policy", SrcAttr: "arn"},
				{Attr: "user", SrcType: "aws_iam_user", SrcAttr: "name"},
			},
			"aws_iam_user_group_membership": {
				{Attr: "groups", SrcType: "aws_iam_group", SrcAttr: "name"},
				{Attr: "user", SrcType: "aws_iam_user", SrcAttr: "name"},
			},
			"azurerm_network_interface": {
				{Attr: "ip_configuration.public_ip_address_id", SrcType: "azurerm_public_ip", SrcAttr: "id"},
				{Attr: "ip_configuration.subnet_id", SrcType: "azurerm_subnet", SrcAttr: "id"},
				{Attr: "resource_group_name", SrcType: "azurerm_resource_group", SrcAttr: "name"},
			},
		},
	}
	var b bytes.Buffer
	log.SetOutput(&b)
	defer log.SetOutput(os.Stderr)

	// Parse
	var p Parser
	assert.Equal(t, want, p.ParseDir(dir).Model())
	assert.Equal(t,
		`Attribute with 0 simple values: azurerm_network_interface.location = ["%%0000-${azurerm_resource_group.test.location}"]`,
		strings.TrimSpace(b.String()))

	// Filter
	p.Apply(map[string]bool{".location": false})
	p.Call(func(t *Attr) bool { return t.Type != "aws_iam_user_group_membership" })
	b.Reset()
	delete(want.DepMap, "aws_iam_user_group_membership")
	assert.Equal(t, want, p.Model())
	assert.Empty(t, b.Bytes())
}

func TestParserSchema(t *testing.T) {
	s := test.Provider().(*schema.Provider)
	r := s.ResourcesMap
	r1 := r["test_resource"]
	r2 := r["test_resource_gh12183"]
	tests := []*struct {
		attr   string
		scalar bool
		str    bool
		schema AttrSchema
	}{
		{},
		{attr: "test_resource", schema: AttrSchema{
			Resource: r1,
		}},
		{attr: "test_resource.id", scalar: true, str: true, schema: AttrSchema{
			Schema:   idHier[0],
			Resource: r1,
		}},
		{attr: "test_resource.required", scalar: true, str: true, schema: AttrSchema{
			Schema:   r1.Schema["required"],
			Resource: r1,
		}},
		{attr: "test_resource.set", str: true, schema: AttrSchema{
			Schema:   r1.Schema["set"].Elem.(*schema.Schema),
			Resource: r1,
			Hier:     []*schema.Schema{r1.Schema["set"]},
		}},
		{attr: "test_resource.map_that_look_like_set", schema: AttrSchema{
			Schema:   r1.Schema["map_that_look_like_set"],
			Resource: r1,
		}},
		{attr: "test_resource.map_that_look_like_set.key", scalar: true, str: true, schema: AttrSchema{
			Schema:   r1.Schema["map_that_look_like_set"].Elem.(*schema.Schema),
			Resource: r1,
			Hier:     []*schema.Schema{r1.Schema["map_that_look_like_set"]},
		}},
		{attr: "test_resource_gh12183.config", schema: AttrSchema{
			Schema:   r2.Schema["config"],
			Resource: r2,
		}},
		{attr: "test_resource_gh12183.config.name", str: true, schema: AttrSchema{
			Schema:   r2.Schema["config"].Elem.(*schema.Resource).Schema["name"],
			Resource: r2,
			Hier:     []*schema.Schema{r2.Schema["config"]},
		}},
		{attr: "test_resource_gh12183.config.rules", str: true, schema: AttrSchema{
			Schema:   r2.Schema["config"].Elem.(*schema.Resource).Schema["rules"].Elem.(*schema.Schema),
			Resource: r2,
			Hier:     []*schema.Schema{r2.Schema["config"], r2.Schema["config"].Elem.(*schema.Resource).Schema["rules"]},
		}},
	}
	p := Parser{Provider: s}
	for _, tc := range tests {
		if tc.schema.Schema != nil {
			tc.schema.Hier = append(tc.schema.Hier, tc.schema.Schema)
		}
		assert.Equal(t, tc.schema, p.Schema(splitAttr(tc.attr)), "%+v", tc)
		assert.Equal(t, tc.scalar, tc.schema.IsScalar(), "%+v", tc)
		assert.Equal(t, tc.str, tc.schema.IsString(), "%+v", tc)
	}
}

const _ = `%s
resource "azurerm_network_interface" "%s" {
  name                = "acctestni-%v"
  location            = "%%%.1f-${azurerm_resource_group.test.location}"
  resource_group_name = "${azurerm_resource_group.test.name}"
  bool_attr           = %[1]s
  quote_attr          = %q

  ip_configuration {
    name                          = "testconfiguration1"
    subnet_id                     = "${azurerm_subnet.test.id}"
    private_ip_address_allocation = "static"
    private_ip_address            = "10.0.2.9"
    public_ip_address_id          = "${azurerm_public_ip.test.id}"
  }

  heredoc = <<EOF
%sEOF
}
%s`
