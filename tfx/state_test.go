package tfx

import (
	"io/ioutil"
	"log"
	"os"
	"testing"

	"github.com/LuminalHQ/cloudcover/x/az"
	tf "github.com/hashicorp/terraform/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	log.SetOutput(ioutil.Discard)
	os.Exit(m.Run())
}

func TestNewState(t *testing.T) {
	have := NewState()
	want := tf.NewState()
	have.Init()
	want.Lineage = have.Lineage
	assert.Equal(t, want, have)
}

func TestAddSub(t *testing.T) {
	a := NewState()
	a.RootModule().Resources["a.a"] = &tf.ResourceState{Type: "a"}

	b := NewState()
	b.RootModule().Resources["b.b"] = &tf.ResourceState{Type: "b"}

	ab := NewState()
	ab.RootModule().Resources["a.a"] = &tf.ResourceState{Type: "a"}
	ab.RootModule().Resources["b.b"] = &tf.ResourceState{Type: "b"}

	orig := DeepCopy(a).(*tf.State)
	AddState(a, b)
	assert.NotEqual(t, orig, a)
	assert.Equal(t, ab, a)
	SubState(a, b)
	assert.Equal(t, orig, a)
}

func TestDeepCopy(t *testing.T) {
	s := NewState()
	s.RootModule().Resources["a.a"] = &tf.ResourceState{Type: "a"}
	c := DeepCopy(s).(*tf.State)
	assert.Equal(t, s, c)
	c.RootModule().Resources["a.a"].Type = "b"
	assert.NotEqual(t, s, c)
}

func TestMakeName(t *testing.T) {
	tests := []*struct{ in, want string }{
		{"_", "_"},
		{"--", "_-"},
		{"-_-", "_-"},
		{"_--", "_--"},
		{"_/a/b-1//2.3$", "_a_b-1_2_3_"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, MakeName(tc.in), "%+v", tc)
	}
	assert.Panics(t, func() { MakeName("") })
}

func TestNormStateKeys(t *testing.T) {
	rg := &tf.ResourceState{
		Type: "azurerm_resource_group",
		Primary: &tf.InstanceState{
			ID: "/subscriptions/" + az.NilGUID + "/resourceGroups/tf-test-rg",
		},
		Provider: "provider.azurerm",
	}
	have := NewState()
	have.RootModule().Resources["azurerm_resource_group.rg"] = rg
	want := NewState()
	normKey := "azurerm_resource_group.provider_azurerm_subscriptions_" + az.NilGUID + "_resourceGroups_tf-test-rg"
	want.RootModule().Resources[normKey] = DeepCopy(rg).(*tf.ResourceState)
	st, err := NormStateKeys(have)
	require.NoError(t, err)
	require.NoError(t, st.Apply(have))
	assert.Equal(t, want, have)
}

func TestStateTransform(t *testing.T) {
	orig := NewState()
	orig.Modules = []*tf.ModuleState{{
		Path: []string{"root"},
		Resources: map[string]*tf.ResourceState{
			"a.a": {
				Type:         "a",
				Dependencies: []string{"d.d", "e.e", "unknown.resource"},
			},
			"b.b": {
				Type:         "b",
				Dependencies: []string{"a.a", "c.c", "d.d", "e.e"},
			},
			"c.c": {
				Type:         "c",
				Dependencies: []string{"a.a", "e.e"},
			},
			"d.d": {Type: "d"},
			"e.e": {Type: "e"},
		},
	}}
	st := StateTransform{
		// Swap a and b
		"module.root.a.a": "b.b",
		"b.b":             "module.root.a.a",

		// Replace e with c
		"c.c": "e.e",

		// Delete d
		"d.d": "",
	}
	have := DeepCopy(orig).(*tf.State)
	want := DeepCopy(orig).(*tf.State)
	r := want.Modules[0].Resources
	r["a.a"].Dependencies = []string{"e.e", "unknown.resource"}
	r["b.b"].Dependencies = []string{"b.b", "e.e"}
	r["c.c"].Dependencies = []string{"b.b"}
	delete(r, "d.d")
	r["a.a"], r["b.b"], r["e.e"] = r["b.b"], r["a.a"], r["c.c"]
	delete(r, "c.c")
	require.NoError(t, st.Apply(have))
	require.Equal(t, want, have)

	// TODO: Module tests
}
