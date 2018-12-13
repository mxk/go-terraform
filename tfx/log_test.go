package tfx

import (
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	tf "github.com/hashicorp/terraform/terraform"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	DisableLogging()
	os.Exit(m.Run())
}

func TestLog(t *testing.T) {
	defer func() {
		DisableLogging()
		log.SetFlags(log.LstdFlags)
	}()
	var b strings.Builder
	log.SetFlags(0)

	assert.Error(t, SetLogFilter(&b, "INVALID", false))

	require.NoError(t, SetLogFilter(&b, "", false))
	tf.NewState()
	log.Print("passthrough")
	require.Equal(t, "passthrough\n", b.String())
	b.Reset()

	require.NoError(t, SetLogFilter(&b, "", true))
	tf.NewState()
	log.Print("passthrough")
	require.Empty(t, b.String())
	b.Reset()

	require.NoError(t, SetLogFilter(&b, "DEBUG", false))
	s := tf.NewState()
	want := fmt.Sprintf("[DEBUG] New state was assigned lineage %q\n", s.Lineage)
	require.Equal(t, want, b.String())
	b.Reset()

	require.NoError(t, SetLogFilter(&b, "info", false))
	tf.NewState()
	log.Print("passthrough")
	require.Equal(t, "passthrough\n", b.String())
	b.Reset()

	require.NoError(t, SetLogFilter(&b, "INFO", true))
	tf.NewState()
	log.Print("passthrough")
	require.Empty(t, b.String())
	b.Reset()
}
