// +build generate

package main

import (
	"log"
	"strings"

	"github.com/LuminalHQ/cloudcover/x/tfx/depgen"
	"github.com/terraform-providers/terraform-provider-aws/aws"
)

func main() {
	var p depgen.Parser
	dir := depgen.ModuleDir(aws.Provider)
	p.ParseDir(dir).Filter(filter).Model().Write()
}

// TODO: Inspect log output and add custom rules

func filter(v *depgen.AttrVals) bool {
	if !strings.HasPrefix(v.Type, "aws_") {
		return false
	}
	if len(v.Complex) > 0 {
		log.Printf("Complex values: %v\n", v)
		return false
	}
	return true
}
