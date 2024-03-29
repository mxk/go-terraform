package tfx

import (
	"reflect"
	"testing"

	"github.com/hashicorp/terraform/builtin/providers/test"
	"github.com/hashicorp/terraform/helper/schema"
	"github.com/stretchr/testify/require"
)

func TestConfig(t *testing.T) {
	d, err := Config(test.Provider().(*schema.Provider).Schema, map[string]interface{}{
		"label": "abc",
	})
	require.NoError(t, err)
	require.Equal(t, "abc", d.Get("label"))
}

func TestProviderFields(t *testing.T) {
	// Changes to schema.Provider fields may require updates to providerMode
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
