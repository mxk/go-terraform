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

// SetLogFilter configures Terraform log filter. Since all Terraform components
// use the default logger (ugh... why?!?), this may affect other code as well.
func SetLogFilter(w io.Writer, level string) error {
	if w == nil {
		w = os.Stderr
	}
	filter := &logutils.LevelFilter{Levels: logging.ValidLevels, Writer: w}
	if level != "" {
		level = strings.ToUpper(level)
		for _, valid := range filter.Levels {
			if level == string(valid) {
				filter.MinLevel = valid
				break
			}
		}
		if filter.MinLevel == "" {
			return fmt.Errorf("tfx: invalid log level %q (must be one of: %v)",
				level, logging.ValidLevels)
		}
	}
	log.SetOutput(filter)
	return nil
}
