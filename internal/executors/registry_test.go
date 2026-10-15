package executors

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func TestDockerConfigJSON(t *testing.T) {
	auth := &agentv1.RegistryAuth{Host: "ghcr.io", Username: "octo", Password: `p"ss{w}rd`}
	raw := dockerConfigJSON(auth)

	var parsed struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("config.json is not valid JSON: %v\n%s", err, raw)
	}
	entry, ok := parsed.Auths["ghcr.io"]
	if !ok {
		t.Fatalf("missing auths entry for host: %s", raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		t.Fatalf("auth is not base64: %v", err)
	}
	if string(decoded) != `octo:p"ss{w}rd` {
		t.Fatalf("auth decodes to %q", decoded)
	}
}
