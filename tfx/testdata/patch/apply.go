package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/mxk/go-terraform/tfx"
	"github.com/hashicorp/terraform/builtin/providers/test"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <config-file> ...\n", os.Args[0])
		os.Exit(2)
	}
	var ctx tfx.Ctx
	ctx.SetProvider("test", test.Provider())
	for _, config := range os.Args[1:] {
		if !strings.HasSuffix(config, ".tf") {
			panic("invalid config file: " + config)
		}
		t, err := tfx.LoadModule(config)
		if err != nil {
			panic(err)
		}
		s, err := ctx.Apply(t, tfx.NewState())
		if err != nil {
			panic(err)
		}
		if err = tfx.WriteStateFile(config+"state", s); err != nil {
			panic(err)
		}
	}
}
