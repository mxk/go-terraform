package tfx

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform/builtin/providers/test"
	"github.com/stretchr/testify/require"
)

func TestPatch(t *testing.T) {
	root := testDataDir("patch")
	commonState, err := ReadState(filepath.Join(root, "common.tfstate"))
	require.NoError(t, err)

	files, err := ioutil.ReadDir(root)
	require.NoError(t, err)

	var ctx Ctx
	ctx.SetProvider("test", test.Provider())
	for _, fi := range files {
		config := fi.Name()
		if !strings.HasSuffix(config, ".tf") {
			continue
		}

		m, err := LoadModule(filepath.Join(root, config))
		require.NoError(t, err, "%s", config)

		s, err := ReadState(filepath.Join(root, config+"state"))
		if err != nil {
			if !os.IsNotExist(err) {
				require.NoError(t, err)
			}
			s = commonState
		}

		d, err := ctx.Diff(m, s)
		require.NoError(t, err, "%s", config)
		fmt.Printf("%s:\n%v\n\n", config, d)

		want, err := ctx.Apply(m, s)
		require.True(t, want != s, "%s", config)
		require.NoError(t, err, "%s", config)

		have, err := ctx.Patch(s, d)
		require.True(t, have != s, "%s", config)
		require.NoError(t, err, "%s", config)

		require.Equal(t, want, have, "%s", config)
	}
}
