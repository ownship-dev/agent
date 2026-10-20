package executors

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// fakeGit records each git call and fails the ones whose args contain failOn.
type fakeGit struct {
	checkout bool
	failOn   string
	calls    []string
	removed  bool
}

func (f *fakeGit) git(ctx context.Context, args []string, _ []string) *exec.Cmd {
	call := strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return exec.CommandContext(ctx, "false")
	}
	return exec.CommandContext(ctx, "true")
}

func (f *fakeGit) auth(context.Context, *agentv1.RepoSource, string) ([]string, []string, func(), error) {
	return []string{"-c", "auth"}, nil, func() {}, nil
}
func (f *fakeGit) hasCheckout(context.Context, string) bool { return f.checkout }
func (f *fakeGit) removeAll(context.Context, string) error  { f.removed = true; return nil }

func TestGitSyncUpdatesACachedCheckoutInPlace(t *testing.T) {
	h := &fakeGit{checkout: true}
	if err := gitSync(context.Background(), h, &agentv1.RepoSource{Ref: "main", CloneUrl: "https://x/r"}, "/w", true, func(agentv1.LogStream, string) {}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-c auth -C /w fetch --depth 1 --end-of-options origin main",
		"-C /w reset --hard FETCH_HEAD",
		"-C /w clean -fd",
	}
	if strings.Join(h.calls, "\n") != strings.Join(want, "\n") || h.removed {
		t.Fatalf("calls %q, removed %v", h.calls, h.removed)
	}
}

func TestGitSyncReclonesWhenTheUpdateFailsOrCacheIsOff(t *testing.T) {
	clone := "-c auth clone --depth 1 --branch main --end-of-options https://x/r /w"
	for _, tc := range []struct {
		name  string
		h     *fakeGit
		cache bool
	}{
		{"failed fetch", &fakeGit{checkout: true, failOn: "fetch"}, true},
		{"cache off", &fakeGit{checkout: true}, false},
	} {
		if err := gitSync(context.Background(), tc.h, &agentv1.RepoSource{Ref: "main", CloneUrl: "https://x/r"}, "/w", tc.cache, func(agentv1.LogStream, string) {}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !tc.h.removed || tc.h.calls[len(tc.h.calls)-1] != clone {
			t.Fatalf("%s: calls %q, removed %v", tc.name, tc.h.calls, tc.h.removed)
		}
	}
}

func TestLXCGitRunsInsideTheContainer(t *testing.T) {
	cmd := lxcGit{vmid: "105"}.git(context.Background(), []string{"-C", "/root/app", "clean", "-fd"}, nil)
	if got := strings.Join(cmd.Args, " "); got != "pct exec 105 -- git -C /root/app clean -fd" {
		t.Fatalf("argv %q", got)
	}
}
