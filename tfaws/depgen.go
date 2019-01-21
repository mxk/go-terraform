// +build generate

package main

import (
	"github.com/mxk/cloudcover/x/tfx/depgen"
	"github.com/terraform-providers/terraform-provider-aws/aws"
)

// TODO: Inspect log output and add custom rules
// TODO: Find validateArn in schemas to detect missing references

func main() {
	var p depgen.Parser
	p.Parse(aws.Provider).Model().Write()
}
