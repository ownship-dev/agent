package executors

import (
	"fmt"
	"strings"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// envFileContent builds an --env-file body; a file keeps secrets off argv.
func envFileContent(env []*agentv1.EnvEntry, port uint32) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PORT=%d\n", port)
	for _, e := range env {
		fmt.Fprintf(&b, "%s=%s\n", e.GetKey(), e.GetValue())
	}
	return b.String()
}
