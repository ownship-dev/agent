package compose

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func opts() Options {
	return Options{
		Env:              map[string]string{"TAG": "1.2", "PRIV": "true", "MODE": "host"},
		PrimaryService:   "web",
		PrimaryContainer: "ownship-wl1",
		PrimaryPublish:   "127.0.0.1:34567:3000",
		EnvFile:          "/tmp/ownship-env",
		LinkNetwork:      "ownship",
		AllowBuild:       true,
		WorkloadID:       "wl1",
		Volume: func(name string) map[string]any {
			return map[string]any{"name": "ownship-wl1-" + name}
		},
	}
}

func prepare(t *testing.T, src string, o Options) Result {
	t.Helper()
	r, err := Prepare([]byte(src), o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func refusedWith(t *testing.T, src, want string) {
	t.Helper()
	r := prepare(t, src, opts())
	if len(r.Violations) == 0 {
		t.Fatalf("accepted, want refusal containing %q", want)
	}
	for _, v := range r.Violations {
		if strings.Contains(v.Msg, want) {
			return
		}
	}
	t.Fatalf("violations %v do not mention %q", r.Violations, want)
}

func TestRefusesWhatReachesPastTheWorkload(t *testing.T) {
	cases := map[string]string{
		"privileged":        "services:\n  web:\n    image: x\n    privileged: true\n",
		"network_mode":      "services:\n  web:\n    image: x\n    network_mode: host\n",
		"pid":               "services:\n  web:\n    image: x\n    pid: host\n",
		"devices":           "services:\n  web:\n    image: x\n    devices: [/dev/sda]\n",
		"capability":        "services:\n  web:\n    image: x\n    cap_add: [SYS_ADMIN]\n",
		"security_opt":      "services:\n  web:\n    image: x\n    security_opt: [seccomp:unconfined]\n",
		"mounts a host":     "services:\n  web:\n    image: x\n    volumes: [/etc:/host-etc]\n",
		"./data\" mounts":   "services:\n  web:\n    image: x\n    volumes: [./data:/data]\n",
		"bind mounts":       "services:\n  web:\n    image: x\n    volumes: [{type: bind, source: /, target: /h}]\n",
		"env_file":          "services:\n  web:\n    image: x\n    env_file: /root/.ownship/token\n",
		"extends":           "services:\n  web:\n    image: x\n    extends: {file: /etc/x.yml, service: a}\n",
		"include":           "include: [/etc/other.yml]\nservices:\n  web:\n    image: x\n",
		"Ownship manages":   "services:\n  web:\n    image: x\n    volumes: [d:/d]\nvolumes:\n  d:\n    driver_opts: {type: none, o: bind, device: /}\n",
		"external":          "services:\n  web:\n    image: x\nvolumes:\n  d:\n    external: true\n",
		"only content":      "services:\n  web:\n    image: x\nsecrets:\n  s:\n    file: /etc/shadow\n",
		"reserved":          "services:\n  web:\n    image: x\n    labels: {com.docker.compose.project: other}\n",
		"logging driver":    "services:\n  web:\n    image: x\n    logging: {driver: syslog}\n",
		"devices is not":    "services:\n  web:\n    image: x\n    deploy: {resources: {reservations: {devices: [{capabilities: [gpu]}]}}}\n",
		"inside the repo":   "services:\n  web:\n    build: ../../etc\n",
		"cache_from":        "services:\n  web:\n    build: {context: ., cache_from: [type=local,src=/root]}\n",
		"exactly one":       "services:\n  web:\n    image: x\n    scale: 3\n",
		"not defined":       "services:\n  api:\n    image: x\n",
		"name \"ownship":    "services:\n  web:\n    image: x\n  ownship-x:\n    image: y\n",
		"keys must be":      "services:\n  web:\n    image: x\n    deploy: {1: x}\n",
		"post_start":        "services:\n  web:\n    image: x\n    post_start: [{command: id, privileged: true}]\n",
		"network \"other\"": "services:\n  web:\n    image: x\n    networks: [other]\n",
		"only the bridge":   "services:\n  web:\n    image: x\nnetworks:\n  n:\n    driver: host\n",
	}
	for want, src := range cases {
		t.Run(want, func(t *testing.T) { refusedWith(t, src, want) })
	}
}

// Interpolation happens before validation, so a variable can't smuggle a setting in.
func TestInterpolationCannotSmuggleSettings(t *testing.T) {
	refusedWith(t, "services:\n  web:\n    image: x\n    cap_add: [\"${MODE}\"]\n", "capability")
	refusedWith(t, "services:\n  web:\n    image: x\n    volumes: [\"${UNSET:-/etc}:/e\"]\n", "host path")
}

// Anchors and merge keys are resolved before the checks run.
func TestMergeKeysAreResolvedBeforeChecking(t *testing.T) {
	src := "x-base: &base\n  privileged: true\nservices:\n  web:\n    <<: *base\n    image: x\n"
	refusedWith(t, src, "privileged")
}

func TestReportsLineNumbers(t *testing.T) {
	r := prepare(t, "services:\n  web:\n    image: x\n    privileged: true\n", opts())
	if len(r.Violations) != 1 || r.Violations[0].Line != 4 {
		t.Fatalf("violations = %+v", r.Violations)
	}
}

func TestBuildNeedsARepository(t *testing.T) {
	o := opts()
	o.AllowBuild = false
	r := prepare(t, "services:\n  web:\n    build: .\n", o)
	if len(r.Violations) == 0 || !strings.Contains(r.Violations[0].Msg, "needs a repository") {
		t.Fatalf("violations = %+v", r.Violations)
	}
}

func TestAddsOwnshipWiring(t *testing.T) {
	src := `
services:
  web:
    build: .
    ports: ["3000:3000"]
    environment:
      VERSION: ${TAG}
      LITERAL: "a$$b"
    volumes: [data:/data]
  db:
    image: postgres:16
    container_name: ownship-someone-else
    networks: [backend]
volumes:
  data: {}
networks:
  backend: {}
`
	r := prepare(t, src, opts())
	if len(r.Violations) > 0 {
		t.Fatalf("refused: %v", r.Violations)
	}
	if len(r.Notes) != 2 {
		t.Fatalf("notes = %v", r.Notes)
	}
	var out map[string]any
	if err := yaml.Unmarshal(r.File, &out); err != nil {
		t.Fatal(err)
	}
	svcs := out["services"].(map[string]any)
	web := svcs["web"].(map[string]any)
	db := svcs["db"].(map[string]any)

	if web["container_name"] != "ownship-wl1" || db["container_name"] != nil {
		t.Fatalf("container names: web=%v db=%v", web["container_name"], db["container_name"])
	}
	if p := web["ports"].([]any); len(p) != 1 || p[0] != "127.0.0.1:34567:3000" {
		t.Fatalf("ports = %v", p)
	}
	if web["env_file"].([]any)[0] != "/tmp/ownship-env" || web["restart"] != "unless-stopped" {
		t.Fatal("primary wiring missing")
	}
	if web["labels"].(map[string]any)["ownship.workload"] != "wl1" || db["labels"].(map[string]any)["ownship.workload"] != "wl1" {
		t.Fatal("workload label missing")
	}
	// Resolved once here, then escaped so compose leaves it alone.
	env := web["environment"].(map[string]any)
	if env["VERSION"] != "1.2" || env["LITERAL"] != "a$$b" {
		t.Fatalf("environment = %v", env)
	}
	if out["volumes"].(map[string]any)["data"].(map[string]any)["name"] != "ownship-wl1-data" {
		t.Fatal("volume not mapped to the agent-owned name")
	}
	link := out["networks"].(map[string]any)[linkNetworkKey].(map[string]any)
	if link["external"] != true || link["name"] != "ownship" {
		t.Fatalf("link network = %v", link)
	}
	nets := web["networks"].([]any)
	if len(nets) != 2 || nets[0] != "default" || nets[1] != linkNetworkKey {
		t.Fatalf("primary networks = %v", nets)
	}
}

func TestInterpolate(t *testing.T) {
	env := map[string]string{"A": "1", "E": ""}
	for in, want := range map[string]string{
		"$A":            "1",
		"${A}":          "1",
		"${B:-x}":       "x",
		"${E:-x}":       "x",
		"${E-x}":        "",
		"${A:+yes}":     "yes",
		"${B:+yes}":     "",
		"$$A":           "$A",
		"${B:-${A}}":    "1",
		"cost: 5$":      "cost: 5$",
		"pre-${A}-post": "pre-1-post",
	} {
		got, err := interpolate(in, env)
		if err != nil || got != want {
			t.Errorf("%q: got %q (%v), want %q", in, got, err, want)
		}
	}
	if _, err := interpolate("${B:?is required}", env); err == nil {
		t.Error("missing required variable accepted")
	}
}

func TestGPUGoesToThePrimaryOnly(t *testing.T) {
	o := opts()
	o.GPU = &GPU{Devices: []string{"/dev/dri"}, Nvidia: true}
	r := prepare(t, "services:\n  web:\n    image: x\n  db:\n    image: y\n", o)
	var out map[string]any
	if err := yaml.Unmarshal(r.File, &out); err != nil {
		t.Fatal(err)
	}
	svcs := out["services"].(map[string]any)
	web, db := svcs["web"].(map[string]any), svcs["db"].(map[string]any)
	if web["devices"] == nil || web["deploy"] == nil || db["devices"] != nil || db["deploy"] != nil {
		t.Fatalf("web=%v db=%v", web, db)
	}
}

// Podman's restart service only starts `always` containers, so the agent can ask for it.
func TestRestartPolicyOption(t *testing.T) {
	o := opts()
	o.Restart = "always"
	r := prepare(t, "services:\n  web:\n    image: nginx\n", o)
	if len(r.Violations) > 0 {
		t.Fatalf("refused: %v", r.Violations)
	}
	if !strings.Contains(string(r.File), "restart: always") {
		t.Fatalf("restart policy not applied:\n%s", r.File)
	}
}
