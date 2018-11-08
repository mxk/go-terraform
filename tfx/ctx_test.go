package tfx

import (
	"testing"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/stretchr/testify/require"
)

func TestBypassCRUD(t *testing.T) {
	p := schema.Provider{
		ResourcesMap: map[string]*schema.Resource{
			"": {
				Schema: map[string]*schema.Schema{
					"attr": {Elem: new(schema.Resource)},
				},
				Read:   func(*schema.ResourceData, interface{}) error { panic("fail") },
				Exists: func(*schema.ResourceData, interface{}) (bool, error) { panic("fail") },
			},
		},
	}
	bypassCRUD(&p)

	r := p.ResourcesMap[""]
	require.NotNil(t, r.Create)
	require.NotNil(t, r.Read)
	require.NotNil(t, r.Update)
	require.NotNil(t, r.Delete)
	require.Nil(t, r.Exists)
	require.NoError(t, r.Read(nil, nil))

	r = r.Schema["attr"].Elem.(*schema.Resource)
	d := r.Data(nil)
	require.NoError(t, r.Create(d, nil))
	require.Equal(t, "unknown", d.Id())
}
