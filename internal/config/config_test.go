package config

import "testing"

// The control plane is a managed SaaS: Load always uses the baked-in URL and
// ignores any environment, so pairing is zero-config and not user-tunable.
func TestLoadControlPlaneURL(t *testing.T) {
	t.Setenv("CONTROL_PLANE_URL", "https://someone-tried-to-override.example")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ControlPlaneURL != ControlPlaneURL {
		t.Fatalf("got %q, want baked-in %q", cfg.ControlPlaneURL, ControlPlaneURL)
	}
}
