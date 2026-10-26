package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/ownship/agent/internal/executors"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

type passwordExec struct {
	executors.Executor
	err error
}

func (p passwordExec) SetRootPassword(context.Context, *agentv1.SetRootPassword) error { return p.err }

// The API waits on this answer, so a failure must come back as a result, never only a log line.
func TestSetRootPasswordAlwaysAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec executors.Executor
		want string
	}{
		{"ok", passwordExec{}, ""},
		{"chpasswd failed", passwordExec{err: errors.New("chpasswd: boom")}, "chpasswd: boom"},
		{"unsupported", passwordExec{}.Executor, "this agent can't set guest passwords"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got *agentv1.RootPasswordResult
			r := New(tc.exec, slog.New(slog.NewTextHandler(io.Discard, nil)))
			r.Handle(context.Background(), &agentv1.Command{
				Id:      "cmd-" + tc.name,
				Payload: &agentv1.Command_SetRootPassword{SetRootPassword: &agentv1.SetRootPassword{RequestId: "req-1"}},
			}, func(e *agentv1.Event) error {
				if res := e.GetRootPasswordResult(); res != nil {
					got = res
				}
				return nil
			})
			if got == nil || got.GetRequestId() != "req-1" || got.GetError() != tc.want {
				t.Fatalf("got %+v, want request req-1 with error %q", got, tc.want)
			}
		})
	}
}
