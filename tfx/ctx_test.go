package tfx

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform/builtin/providers/test"
	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/config/module"
	"github.com/hashicorp/terraform/helper/schema"
	tf "github.com/hashicorp/terraform/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCtx(t *testing.T) {
	renameTypes := func(name string, src map[string]*schema.Resource) {
		tmp := make(map[string]*schema.Resource, len(src))
		for typ, r := range src {
			tmp[name+strings.TrimPrefix(typ, "test")] = r
			delete(src, typ)
		}
		for typ, r := range tmp {
			src[typ] = r
		}
	}
	newFactory := func(name string, cfgCount *int32) tf.ResourceProviderFactory {
		return func() (tf.ResourceProvider, error) {
			p := test.Provider().(*schema.Provider)
			renameTypes(name, p.ResourcesMap)
			renameTypes(name, p.DataSourcesMap)
			p.ConfigureFunc = func(d *schema.ResourceData) (interface{}, error) {
				return atomic.AddInt32(cfgCount, 1), nil
			}
			return p, nil
		}
	}

	var cfgCount [2]int32
	ctx := Ctx{Providers: new(ProviderReg).
		Register("test1", "", newFactory("test1", &cfgCount[0])).
		Register("test2", "", newFactory("test2", &cfgCount[1])),
	}

	s, err := ctx.Apply(loadCfg(t, applyCfg1), nil)
	require.NoError(t, err)
	assert.Equal(t, [2]int32{1, 0}, cfgCount)
	assert.Equal(t, "t1", s.Modules[0].Resources["test1_resource.t1"].Primary.Attributes["required"])

	cfgCount = [2]int32{}
	s, err = ctx.Apply(loadCfg(t, applyCfg2), NewState())
	require.NoError(t, err)
	assert.Equal(t, [2]int32{0, 2}, cfgCount)
	assert.Equal(t, "t2", s.Modules[0].Resources["test2_resource.t2"].Primary.Attributes["required"])
	assert.Equal(t, "t2-alias", s.Modules[0].Resources["test2_resource.t2-alias"].Primary.Attributes["required"])

	cfgCount = [2]int32{}
	s, err = ctx.Apply(loadCfg(t, applyCfg1+applyCfg2), nil)
	require.NoError(t, err)
	assert.Equal(t, [2]int32{1, 2}, cfgCount)
	assert.Equal(t, "t1", s.Modules[0].Resources["test1_resource.t1"].Primary.Attributes["required"])
	assert.Equal(t, "t2", s.Modules[0].Resources["test2_resource.t2"].Primary.Attributes["required"])
	assert.Equal(t, "t2-alias", s.Modules[0].Resources["test2_resource.t2-alias"].Primary.Attributes["required"])
}

func loadCfg(t *testing.T, cfg string) *module.Tree {
	c, err := config.LoadJSON(json.RawMessage(cfg))
	require.NoError(t, err)
	m := module.NewTree("", c)
	err = m.Load(&module.Storage{Mode: module.GetModeNone})
	require.NoError(t, err)
	return m
}

const applyCfg1 = `
resource "test1_resource" "t1" {
	required     = "t1"
	required_map = {x = 0}
}
`

const applyCfg2 = `
provider "test2" {
	alias = "alias"
}

resource "test2_resource" "t2" {
	required     = "t2"
	required_map = {x = 0}
}

resource "test2_resource" "t2-alias" {
	provider     = "test2.alias"
	required     = "t2-alias"
	required_map = {x = 0}
}
`
