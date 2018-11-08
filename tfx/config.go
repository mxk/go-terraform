package tfx

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"strings"

	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/config/module"
)

// LoadModule reads module config from a file or directory ("" or "-" mean
// stdin).
func LoadModule(path string) (*module.Tree, error) {
	var c *config.Config
	var err error
	if path == "" || path == "-" {
		var b []byte
		b, err = ioutil.ReadAll(io.LimitReader(os.Stdin, 64*1024*1024))
		if err == nil {
			c, err = config.LoadJSON(json.RawMessage(b))
		}
	} else if st, err := os.Stat(path); err == nil && st.IsDir() {
		c, err = config.LoadDir(path)
	} else {
		c, err = config.LoadFile(path)
	}
	if err != nil {
		return nil, err
	}
	t := module.NewTree("", c)
	if err = t.Load(&module.Storage{Mode: module.GetModeNone}); err != nil {
		t = nil
	}
	return t, err
}

// TypeProvider returns the provider name prefix for the specified resource or
// data type.
func TypeProvider(typ string) string {
	if i := strings.IndexByte(typ, '_'); i > 0 {
		return typ[:i]
	}
	return ""
}
