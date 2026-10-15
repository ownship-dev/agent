package executors

import (
	"github.com/ownship/agent/internal/compose"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Compose service containers are named osc-<id>-<svc>-N; one whose name merely contains
// "ownship-" must never surface as a workload id, or the reconcile GC would destroy it.
func TestListReportsOnlyPrimaryContainers(t *testing.T) {
	got := workloadIDsFromNames([]string{
		"ownship-p1",
		"ownship-p1-auth",
		"osc-p1-db-1",
		"osc-p1-ownship-cache-1",
		"someone-ownship-x",
		"ownship-p2",
	})
	if want := []string{"p1", "p2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestComposeFileMustStayInsideTheCheckout(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "token")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "compose.yml"), []byte("services: {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo, "evil.yml")); err != nil {
		t.Fatal(err)
	}

	if data, err := readHostCompose(repo, "compose.yml"); err != nil || string(data) != "services: {}" {
		t.Fatalf("in-repo file: %q %v", data, err)
	}
	for _, rel := range []string{"evil.yml", "../" + filepath.Base(outside) + "/token", "/etc/passwd", ""} {
		data, err := readHostCompose(repo, rel)
		if err == nil || strings.Contains(string(data), "s3cret") {
			t.Fatalf("%q was read: %q", rel, data)
		}
	}
}

func TestComposeRefusalNamesEveryViolation(t *testing.T) {
	var logged []string
	log := func(_ agentv1.LogStream, line string) { logged = append(logged, line) }
	_, err := prepareCompose([]byte("services:\n  web:\n    image: x\n    privileged: true\n    pid: host\n"),
		compose.Options{PrimaryService: "web"}, log)
	if err == nil || !strings.Contains(err.Error(), "privileged") || !strings.Contains(err.Error(), "pid") {
		t.Fatalf("err = %v", err)
	}
	if len(logged) != 2 {
		t.Fatalf("logged = %v", logged)
	}
}

// A build context symlinked out of the checkout would make the host the build context.
func TestBuildPathsArePinnedInsideTheCheckout(t *testing.T) {
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	outside := t.TempDir()
	for _, d := range []string{"app", "deploy"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(repo, "app", "Dockerfile"), []byte("FROM scratch"), 0o644)
	_ = os.Symlink(outside, filepath.Join(repo, "evil"))
	_ = os.Symlink(filepath.Join(outside, "Dockerfile"), filepath.Join(repo, "app", "evil.Dockerfile"))
	_ = os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch"), 0o644)

	resolve := hostBuildResolver(repo, filepath.Join(repo, "deploy"))
	ctx, df, err := resolve("../app", "Dockerfile")
	if err != nil || ctx != filepath.Join(repo, "app") || df != filepath.Join(repo, "app", "Dockerfile") {
		t.Fatalf("in-repo build: %q %q %v", ctx, df, err)
	}
	for _, c := range [][2]string{{"../evil", ""}, {"../app", "evil.Dockerfile"}, {"../../..", ""}} {
		if _, _, err := resolve(c[0], c[1]); err == nil {
			t.Fatalf("%v escaped the checkout", c)
		}
	}
}

func TestComposeUpIgnoresTheRepositoryEnvFile(t *testing.T) {
	args := strings.Join(composeUpArgs("wl1", "/tmp/x/.ownship-compose.yml", "/repo/deploy"), " ")
	for _, want := range []string{"-p osc-wl1", "--env-file /dev/null", "--project-directory /repo/deploy", "up -d --build"} {
		if !strings.Contains(args, want) {
			t.Fatalf("%q missing from %q", want, args)
		}
	}
}

func TestSanitizedBuildUsesTheResolvedPaths(t *testing.T) {
	r, err := compose.Prepare([]byte("services:\n  web:\n    build: ./app\n"), compose.Options{
		PrimaryService: "web",
		AllowBuild:     true,
		ResolveBuild: func(c, d string) (string, string, error) {
			return "/real/" + c, "", nil
		},
	})
	if err != nil || len(r.Violations) > 0 {
		t.Fatalf("%v %v", err, r.Violations)
	}
	if !strings.Contains(string(r.File), "context: /real/./app") {
		t.Fatalf("output:\n%s", r.File)
	}
}
