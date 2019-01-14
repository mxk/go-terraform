package tfx

import (
	"strconv"
	"testing"

	"github.com/hashicorp/terraform/builtin/providers/test"
	"github.com/stretchr/testify/assert"
)

func TestDeps(t *testing.T) {
	Providers.Add("test", "", MakeFactory(test.Provider))
	defer delete(Providers, "test")
	deps := make(DepMap)
	deps.Add(DepMap{
		"test_resource": {
			{Attr: "required", SrcType: "test_resource_with_custom_diff", SrcAttr: "required"},
			{Attr: "set", SrcType: "test_resource_with_custom_diff", SrcAttr: "required"},
			{Attr: "list_of_map.key", SrcType: "test_resource_with_custom_diff", SrcAttr: "required"},
		},
	})

	// Create state
	s := NewState()
	m := s.RootModule()
	src, _ := Providers.MakeResources("test_resource_with_custom_diff", AttrGen{
		"#":        3,
		"id":       func(i int) string { return strconv.Itoa(i) },
		"required": func(i int) string { return strconv.Itoa(i) },
	})
	dst, _ := Providers.MakeResources("test_resource", AttrGen{
		"#":  4,
		"id": func(i int) string { return strconv.Itoa(i) },
	})
	for _, r := range append(src, dst...) {
		m.Resources[r.Key] = r.ResourceState
	}

	// Set destination attribute values
	for i := range dst {
		dst[i].Data().Set("required", "0")
	}
	dst[0].Data().Set("required", "x")
	dst[2].Data().Set("set", []interface{}{"1"})
	dst[3].Data().Set("set", []interface{}{"1"})
	dst[3].Data().Set("list_of_map", []interface{}{map[string]interface{}{"key": "2"}})
	for i := range dst {
		dst[i].Primary = dst[i].data.State()
		dst[i].data = nil
	}

	// Infer dependencies
	deps.Infer(s)
	want := []string{
		"test_resource_with_custom_diff.0",
		"test_resource_with_custom_diff.1",
		"test_resource_with_custom_diff.2",
	}
	assert.Equal(t, []string(nil), dst[0].Dependencies)
	assert.Equal(t, want[:1], dst[1].Dependencies)
	assert.Equal(t, want[:2], dst[2].Dependencies)
	assert.Equal(t, want[:3], dst[3].Dependencies)
	for _, s := range src {
		assert.Empty(t, s.Dependencies)
	}
}

func TestUnique(t *testing.T) {
	tests := []*struct {
		have []string
		want []string
	}{
		{},
		{[]string{""}, []string{""}},
		{[]string{"", "", ""}, []string{""}},
		{[]string{"a", "A", "a"}, []string{"A", "a"}},
		{[]string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, unique(tc.have), "%+v", tc)
	}
}
