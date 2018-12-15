package tfx

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/hashicorp/logutils"
	"github.com/hashicorp/terraform/helper/logging"
)

// DisableLogging disables all Terraform logging, while allowing other messages
// through.
func DisableLogging() {
	SetLogFilter(os.Stderr, "", false)
}

// validLevels is updated to contain an empty level to filter out messages
// without a level prefix.
var validLevels = logging.ValidLevels

// SetLogFilter configures Terraform log filter. Since all Terraform components
// use the default logger (ugh... why?!?), this may affect other code as well.
// If requireLevel is true, any log message that does not have a level prefix is
// filtered out.
func SetLogFilter(w io.Writer, level string, requireLevel bool) error {
	if w == nil {
		w = os.Stderr
	}
	const invalid = logutils.LogLevel("INVALID")
	filter := &logutils.LevelFilter{
		Levels:   logging.ValidLevels,
		MinLevel: invalid,
		Writer:   w,
	}
	if requireLevel {
		if validLevels[0] != "" {
			validLevels = make([]logutils.LogLevel, 1+len(logging.ValidLevels))
			copy(validLevels[1:], logging.ValidLevels)
		}
		filter.Levels = validLevels
	}
	if level != "" {
		level = strings.ToUpper(level)
		for _, valid := range filter.Levels {
			if level == string(valid) {
				filter.MinLevel = valid
				break
			}
		}
		if filter.MinLevel == invalid {
			return fmt.Errorf("tfx: invalid log level %q (must be one of: %v)",
				level, logging.ValidLevels)
		}
	}
	log.SetOutput(filter)
	os.Setenv(logging.EnvLog, level) // For logging.LogLevel()
	return nil
}
