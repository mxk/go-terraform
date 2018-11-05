package tfx

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/LuminalHQ/cloudcover/x/cli"
	"github.com/LuminalHQ/cloudcover/x/tfazure"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiff(t *testing.T) {
	tests := []*struct{ state, diff string }{
		{"good.tfstate", ""},
		{"bad.tfstate", `
			MISSING RESOURCE:
			- azurerm_resource_group.rg2

			EXTRA RESOURCE:
			- azurerm_resource_group.rg3

			ATTRIBUTE MISMATCH:
			- azurerm_resource_group.rg1
			  location = "eastus2" (expected: "eastus")

			- azurerm_virtual_network.vnet
			  address_space.0     = "10.0.0.0/16" (expected: "10.0.0.0/8")
			  location            = "eastus2" (expected: "eastus")
			  resource_group_name = "rg3" (expected: "rg1")
		`},
	}
	dir := testDataDir()
	var c Ctx
	c.SetProvider(tfazure.ProviderName, tfazure.Provider(nil))
	m, err := LoadModule(filepath.Join(dir, "cfg.tf"))
	require.NoError(t, err)
	for _, tc := range tests {
		s, err := ReadState(filepath.Join(dir, tc.state))
		require.NoError(t, err)
		d, err := c.Diff(m, s)
		require.NoError(t, err)
		assert.Equal(t, strings.TrimSpace(cli.Dedent(tc.diff)), ExplainDiff(d))
	}
}

func testDataDir() string {
	_, file, _, _ := runtime.Caller(1)
	if file != "" {
		return filepath.Join(filepath.Dir(file), "testdata")
	}
	panic("testdata directory not found")
}
