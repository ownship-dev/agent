package executors

import (
	"context"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// A machine that can't convert still answers, so the migration fails instead of waiting.
func TestDeployerAnswersEveryConvertStepOnDocker(t *testing.T) {
	d := &Deployer{orch: &LocalDocker{}}
	var got []*agentv1.ConvertResult
	emit := func(e *agentv1.Event) error {
		if r := e.GetConvertResult(); r != nil {
			got = append(got, r)
		}
		return nil
	}
	ctx := context.Background()
	_ = d.ConvertPrepare(ctx, &agentv1.ConvertPrepare{MigrationId: "m", WorkloadId: "w"}, emit)
	_ = d.ConvertCommit(ctx, &agentv1.ConvertCommit{MigrationId: "m", WorkloadId: "w"}, emit)
	_ = d.ConvertRollback(ctx, &agentv1.ConvertRollback{MigrationId: "m", WorkloadId: "w"}, emit)
	if len(got) != 3 {
		t.Fatalf("results %v", got)
	}
	for i, step := range []string{"prepare", "commit", "rollback"} {
		r := got[i]
		if r.GetStep() != step || r.GetOk() || r.GetError() == "" || r.GetMigrationId() != "m" || r.GetWorkloadId() != "w" {
			t.Fatalf("result %d = %v", i, r)
		}
	}
}
