package client

import (
	"testing"

	"github.com/ownship/agent/internal/config"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func TestHelloCarriesEngines(t *testing.T) {
	ev := hello(config.Config{}, nil, nil, nil, "", nil, "", []*agentv1.EngineInfo{
		{Kind: agentv1.ContainerEngine_CONTAINER_ENGINE_DOCKER, Version: "27.3.1"},
		{Kind: agentv1.ContainerEngine_CONTAINER_ENGINE_PODMAN, Version: "5.4.0"},
	})
	h := ev.GetHello()
	if len(h.GetEngines()) != 2 {
		t.Fatalf("engines %v", h.GetEngines())
	}
	// Older control planes still read docker_version.
	if h.GetDockerVersion() != "27.3.1" { //nolint:staticcheck
		t.Fatalf("docker_version %q", h.GetDockerVersion()) //nolint:staticcheck
	}
}
