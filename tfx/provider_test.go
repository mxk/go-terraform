package tfx

import (
	"reflect"
	"testing"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/terraform/builtin/providers/test"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/stretchr/testify/require"
)

func TestProviderVersion(t *testing.T) {
	ver := ProviderVersion(test.Provider)
	require.Regexp(t, `^\d+\.\d+\.\d+`, ver)
	v, err := version.NewVersion(ver)
	require.NoError(t, err)
	min, err := version.NewConstraint(">= 0.11.10")
	require.NoError(t, err)
	require.True(t, min.Check(v))
}

func TestConfig(t *testing.T) {
	d, err := Config(test.Provider().(*schema.Provider).Schema, map[string]interface{}{
		"label": "abc",
	})
	require.NoError(t, err)
	require.Equal(t, "abc", d.Get("label"))
}

func TestProviderFields(t *testing.T) {
	// Changes to schema.Provider fields may require updates to disableProvider
	fields := []string{
		"Schema",
		"ResourcesMap",
		"DataSourcesMap",
		"ConfigureFunc",
		"MetaReset",
		"meta",
		"stopMu",
		"stopCtx",
		"stopCtxCancel",
		"stopOnce",
	}
	r := reflect.TypeOf((*schema.Provider)(nil)).Elem()
	require.Equal(t, len(fields), r.NumField())
	for i, f := range fields {
		require.Equal(t, f, r.Field(i).Name)
	}
}

func TestResourceFields(t *testing.T) {
	// Changes to schema.Resource fields may require updates to disableResource
	fields := []string{
		"Schema",
		"SchemaVersion",
		"MigrateState",
		"Create",
		"Read",
		"Update",
		"Delete",
		"Exists",
		"CustomizeDiff",
		"Importer",
		"DeprecationMessage",
		"Timeouts",
	}
	r := reflect.TypeOf((*schema.Resource)(nil)).Elem()
	require.Equal(t, len(fields), r.NumField())
	for i, f := range fields {
		require.Equal(t, f, r.Field(i).Name)
	}
}
