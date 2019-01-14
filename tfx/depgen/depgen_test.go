package depgen

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/LuminalHQ/cloudcover/x/tfx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModuleDir(t *testing.T) {
	d := ModuleDir(require.Contains)
	require.Contains(t, filepath.Base(d), "testify@v1")
}

func TestNewVal(t *testing.T) {
	tests := []*struct {
		have string
		want *Val
	}{
		{},
		{have: "abc"},
		{have: "$${literal}"},
		{have: "${data.resource_type.name.attr}"},
		{
			have: "${resource_type.name.attr}",
			want: &Val{
				Raw:  "${resource_type.name.attr}",
				Type: "resource_type",
				Attr: "attr",
			},
		}, {
			have: "complex${resource_type.name.attr}",
			want: &Val{Raw: "complex${resource_type.name.attr}"},
		}, {
			have: "${resource_type.name.attr[0]}",
			want: &Val{Raw: "${resource_type.name.attr[0]}"},
		},
	}
	for _, tc := range tests {
		v, err := NewVal("", tc.have)
		require.NoError(t, err, "%+v", tc)
		if tc.want == nil {
			assert.Nil(t, v, "%+v", tc)
		} else if assert.NotNil(t, v, "%+v", tc) {
			v.Root = nil
			v.Vars = nil
			assert.Equal(t, tc.want, v, "%+v", tc)
		}
	}
	_, err := NewVal("", "${parse.error")
	require.Error(t, err)
}

func TestParser(t *testing.T) {
	dir := testDir()
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
		`Ignoring attr with 0 simple values: azurerm_network_interface.location = ["%%0000-${azurerm_resource_group.test.location}"]`,
		strings.TrimSpace(b.String()))

	// Filter
	p.Filter(func(v *AttrVals) bool { return v.Attr != "location" })
	b.Reset()
	assert.Equal(t, want, p.Model())
	assert.Empty(t, b.Bytes())
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

func testDir() string {
	fn := runtime.FuncForPC(reflect.ValueOf(TestParser).Pointer())
	file, _ := fn.FileLine(fn.Entry())
	return filepath.Dir(file)
}
