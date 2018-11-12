package tfx

import (
	"fmt"
	"io/ioutil"
	"log"
	"strings"
	"testing"

	tf "github.com/hashicorp/terraform/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLog(t *testing.T) {
	defer func() {
		log.SetFlags(log.LstdFlags)
		log.SetOutput(ioutil.Discard)
	}()
	var b strings.Builder
	log.SetFlags(0)

	assert.Error(t, SetLogFilter(&b, "INVALID"))

	require.NoError(t, SetLogFilter(&b, ""))
	tf.NewState()
	log.Print("passthrough")
	require.Equal(t, "passthrough\n", b.String())
	b.Reset()

	require.NoError(t, SetLogFilter(&b, "DEBUG"))
	s := tf.NewState()
	want := fmt.Sprintf("[DEBUG] New state was assigned lineage %q\n", s.Lineage)
	require.Equal(t, want, b.String())
	b.Reset()

	require.NoError(t, SetLogFilter(&b, "info"))
	tf.NewState()
	log.Print("passthrough")
	require.Equal(t, "passthrough\n", b.String())
	b.Reset()
}
