package executors

import (
	"context"
	"slices"
	"testing"
)

// Proxmox installs either engine inside an LXC on demand, so it always offers both.
func TestProxmoxReportsBothEngines(t *testing.T) {
	var kinds []EngineKind
	for _, e := range newTestProxmox(t).Engines(context.Background()) {
		kinds = append(kinds, e.GetKind())
	}
	if !slices.Equal(kinds, []EngineKind{KindDocker, KindPodman}) {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestDeployerReportsTheOrchestratorsEngines(t *testing.T) {
	d := &Deployer{orch: newTestProxmox(t)}
	if len(d.Engines(context.Background())) != 2 {
		t.Fatal("the deployer must pass the orchestrator's engines through")
	}
	if got := (&Deployer{orch: &finalizingOrch{}}).Engines(context.Background()); got != nil {
		t.Fatalf("an orchestrator without engines reports none, got %v", got)
	}
}
