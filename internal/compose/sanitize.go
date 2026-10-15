// Package compose validates a user's compose file, refusing anything that reaches past the
// workload (host namespaces, devices, bind mounts), then adds Ownship's wiring.
package compose

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type Violation struct {
	Line int
	Msg  string
}

func (v Violation) String() string {
	if v.Line > 0 {
		return fmt.Sprintf("line %d: %s", v.Line, v.Msg)
	}
	return v.Msg
}

type Options struct {
	// Applied to services without one; "" means unless-stopped.
	Restart          string
	Env              map[string]string
	PrimaryService   string
	PrimaryContainer string
	// Port mapping for the primary, e.g. "127.0.0.1:34567:3000"; empty publishes nothing.
	PrimaryPublish string
	// Env file for the primary, keeping secrets out of the compose file.
	EnvFile string
	// The shared bridge linked workloads use.
	LinkNetwork string
	AllowBuild  bool
	// Volume returns the agent-owned definition that replaces a named volume.
	Volume       func(name string) map[string]any
	WorkloadID   string
	MaxFileBytes int
	// ResolveBuild returns absolute build paths, following symlinks and refusing anything
	// outside the repository.
	ResolveBuild func(context, dockerfile string) (absContext, absDockerfile string, err error)
	// GPU goes to the primary only; the file itself may not request devices.
	GPU *GPU
}

type GPU struct {
	Devices    []string
	Nvidia     bool
	GroupVideo bool
}

const linkNetworkKey = "ownship-link"

var (
	serviceNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	volumeNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
)

var allowedServiceKeys = set(
	"image", "build", "command", "entrypoint", "environment", "expose", "healthcheck",
	"depends_on", "restart", "user", "working_dir", "labels", "volumes", "tmpfs", "networks",
	"stop_signal", "stop_grace_period", "init", "read_only", "shm_size", "ulimits", "logging",
	"deploy", "mem_limit", "mem_reservation", "memswap_limit", "cpus", "cpu_shares", "pids_limit",
	"hostname", "domainname", "platform", "pull_policy", "configs", "secrets", "cap_add", "cap_drop",
	"security_opt", "sysctls", "stdin_open", "tty", "extra_hosts", "dns", "dns_search", "links",
	"oom_score_adj", "profiles", "scale", "annotations",
	// Dropped with a note rather than refused: common, and Ownship manages both.
	"ports", "container_name",
)

// Docker's default capability set; anything beyond it is refused.
var allowedCaps = set(
	"CHOWN", "DAC_OVERRIDE", "FSETID", "FOWNER", "MKNOD", "NET_RAW", "SETGID", "SETUID",
	"SETFCAP", "SETPCAP", "NET_BIND_SERVICE", "SYS_CHROOT", "KILL", "AUDIT_WRITE",
)

var allowedBuildKeys = set("context", "dockerfile", "dockerfile_inline", "args", "target", "labels", "shm_size", "tags", "pull", "no_cache")

// Result is the file to run, or why it can't run. Notes list replaced settings.
type Result struct {
	File       []byte
	Violations []Violation
	Notes      []string
}

func refused(msg string) Result { return Result{Violations: []Violation{{Msg: msg}}} }

func Prepare(src []byte, o Options) (Result, error) {
	if o.MaxFileBytes > 0 && len(src) > o.MaxFileBytes {
		return refused(fmt.Sprintf("compose file is larger than %d bytes", o.MaxFileBytes)), nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(src, &root); err != nil {
		return refused("not valid YAML: " + err.Error()), nil
	}
	// Decoding resolves anchors, aliases and merge keys, so validation sees what compose sees.
	var raw any
	if err := yaml.Unmarshal(src, &raw); err != nil {
		return refused("not valid YAML: " + err.Error()), nil
	}
	resolved, err := interpolateAll(raw, o.Env)
	if err != nil {
		return refused(err.Error()), nil
	}
	doc, ok := resolved.(map[string]any)
	if !ok {
		return refused("a compose file must be a mapping"), nil
	}

	c := &checker{root: &root, opts: o}
	c.checkDocument(doc)
	if len(c.violations) > 0 {
		sort.SliceStable(c.violations, func(i, j int) bool { return c.violations[i].Line < c.violations[j].Line })
		return Result{Violations: c.violations}, nil
	}

	notes := augment(doc, o)
	out, err := yaml.Marshal(escapeDollars(doc))
	if err != nil {
		return Result{}, err
	}
	return Result{File: out, Notes: notes}, nil
}

type checker struct {
	root       *yaml.Node
	opts       Options
	violations []Violation
}

func (c *checker) fail(msg string, path ...string) {
	c.violations = append(c.violations, Violation{Line: lineOf(c.root, path...), Msg: msg})
}

func (c *checker) checkDocument(doc map[string]any) {
	for key := range doc {
		switch {
		case key == "services", key == "volumes", key == "networks", key == "configs", key == "secrets":
		case key == "version", strings.HasPrefix(key, "x-"):
		default:
			c.fail(fmt.Sprintf("top-level %q is not allowed", key), key)
		}
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 {
		c.fail("the file defines no services", "services")
		return
	}
	volumes := c.checkVolumes(doc["volumes"])
	networks := c.checkNetworks(doc["networks"])
	configs := c.checkFileObjects(doc["configs"], "configs")
	secrets := c.checkFileObjects(doc["secrets"], "secrets")

	if _, ok := services[c.opts.PrimaryService]; !ok {
		c.fail(fmt.Sprintf("the primary service %q is not defined", c.opts.PrimaryService), "services")
	}
	for name, raw := range services {
		svc, ok := raw.(map[string]any)
		if !ok {
			c.fail("service must be a mapping", "services", name)
			continue
		}
		if !serviceNameRE.MatchString(name) || strings.HasPrefix(name, "ownship") {
			c.fail(fmt.Sprintf("service name %q is not allowed", name), "services", name)
		}
		c.checkService(name, svc, volumes, networks, configs, secrets)
	}
}

func (c *checker) checkService(name string, svc map[string]any, volumes, networks, configs, secrets map[string]bool) {
	at := func(key string) []string { return []string{"services", name, key} }
	for key := range svc {
		if !allowedServiceKeys[key] && !strings.HasPrefix(key, "x-") {
			c.fail(fmt.Sprintf("service %s: %q is not allowed", name, key), at(key)...)
		}
	}
	if name == c.opts.PrimaryService {
		if n, ok := svc["scale"]; ok && n != 1 {
			c.fail("the primary service runs exactly one container", at("scale")...)
		}
	}
	if b, ok := svc["build"]; ok {
		if !c.opts.AllowBuild {
			c.fail(fmt.Sprintf("service %s: build needs a repository; use an image", name), at("build")...)
		} else if b != nil {
			c.checkBuild(name, svc)
		}
	} else if _, ok := svc["image"]; !ok {
		c.fail(fmt.Sprintf("service %s: needs an image or a build", name), "services", name)
	}
	for _, capability := range stringList(svc["cap_add"]) {
		if !allowedCaps[strings.TrimPrefix(strings.ToUpper(capability), "CAP_")] {
			c.fail(fmt.Sprintf("service %s: capability %s is not allowed", name, capability), at("cap_add")...)
		}
	}
	for _, opt := range stringList(svc["security_opt"]) {
		if opt != "no-new-privileges:true" && opt != "no-new-privileges=true" && opt != "no-new-privileges" {
			c.fail(fmt.Sprintf("service %s: security_opt %q is not allowed", name, opt), at("security_opt")...)
		}
	}
	if logging, ok := svc["logging"].(map[string]any); ok {
		if d, _ := logging["driver"].(string); d != "" && d != "json-file" && d != "local" && d != "none" {
			c.fail(fmt.Sprintf("service %s: logging driver %q is not allowed", name, d), at("logging")...)
		}
	}
	if deploy, ok := svc["deploy"].(map[string]any); ok {
		c.checkDeploy(name, deploy)
	}
	for key := range labelKeys(svc["labels"]) {
		if strings.HasPrefix(key, "com.docker.") || strings.HasPrefix(key, "ownship.") {
			c.fail(fmt.Sprintf("service %s: label %q is reserved", name, key), at("labels")...)
		}
	}
	c.checkServiceVolumes(name, svc["volumes"], volumes)
	for _, n := range networkNames(svc["networks"]) {
		if n != "default" && !networks[n] {
			c.fail(fmt.Sprintf("service %s: network %q is not declared in this file", name, n), at("networks")...)
		}
	}
	for _, ref := range objectRefs(svc["configs"]) {
		if !configs[ref] {
			c.fail(fmt.Sprintf("service %s: config %q is not declared in this file", name, ref), at("configs")...)
		}
	}
	for _, ref := range objectRefs(svc["secrets"]) {
		if !secrets[ref] {
			c.fail(fmt.Sprintf("service %s: secret %q is not declared in this file", name, ref), at("secrets")...)
		}
	}
}

func (c *checker) checkBuild(name string, svc map[string]any) {
	at := []string{"services", name, "build"}
	switch b := svc["build"].(type) {
	case string:
		if !safeRelPath(b) {
			c.fail(fmt.Sprintf("service %s: build context must be a path inside the repository", name), at...)
			return
		}
		build := map[string]any{"context": b}
		svc["build"] = build
		c.resolveBuild(name, build, at)
	case map[string]any:
		for key := range b {
			if !allowedBuildKeys[key] {
				c.fail(fmt.Sprintf("service %s: build %q is not allowed", name, key), at...)
			}
		}
		if ctx, ok := b["context"]; ok {
			if s, _ := ctx.(string); !safeRelPath(s) {
				c.fail(fmt.Sprintf("service %s: build context must be a path inside the repository", name), at...)
			}
		}
		if df, ok := b["dockerfile"]; ok {
			if s, _ := df.(string); !safeRelPath(s) {
				c.fail(fmt.Sprintf("service %s: dockerfile must be a path inside the build context", name), at...)
				return
			}
		}
		c.resolveBuild(name, b, at)
	default:
		c.fail(fmt.Sprintf("service %s: build must be a path or a mapping", name), at...)
	}
}

func (c *checker) resolveBuild(name string, build map[string]any, at []string) {
	if c.opts.ResolveBuild == nil || len(c.violations) > 0 {
		return
	}
	ctx, _ := build["context"].(string)
	if ctx == "" {
		ctx = "."
	}
	df, _ := build["dockerfile"].(string)
	absCtx, absDf, err := c.opts.ResolveBuild(ctx, df)
	if err != nil {
		c.fail(fmt.Sprintf("service %s: %v", name, err), at...)
		return
	}
	build["context"] = absCtx
	if absDf != "" {
		build["dockerfile"] = absDf
	}
}

func (c *checker) checkDeploy(name string, deploy map[string]any) {
	at := []string{"services", name, "deploy"}
	for key, val := range deploy {
		switch key {
		case "restart_policy", "labels":
		case "replicas":
			if name == c.opts.PrimaryService && val != 1 {
				c.fail("the primary service runs exactly one container", at...)
			}
		case "resources":
			res, _ := val.(map[string]any)
			for _, kind := range []string{"limits", "reservations"} {
				part, _ := res[kind].(map[string]any)
				for k := range part {
					if k != "cpus" && k != "memory" && k != "pids" {
						c.fail(fmt.Sprintf("service %s: deploy.resources.%s.%s is not allowed", name, kind, k), at...)
					}
				}
			}
			for k := range res {
				if k != "limits" && k != "reservations" {
					c.fail(fmt.Sprintf("service %s: deploy.resources.%s is not allowed", name, k), at...)
				}
			}
		default:
			c.fail(fmt.Sprintf("service %s: deploy.%s is not allowed", name, key), at...)
		}
	}
}

// Named volumes and tmpfs only: a bind mount exposes part of the host.
func (c *checker) checkServiceVolumes(name string, raw any, declared map[string]bool) {
	at := []string{"services", name, "volumes"}
	list, _ := raw.([]any)
	for _, item := range list {
		switch v := item.(type) {
		case string:
			parts := strings.Split(v, ":")
			if len(parts) == 1 {
				if !strings.HasPrefix(parts[0], "/") {
					c.fail(fmt.Sprintf("service %s: volume %q needs an absolute container path", name, v), at...)
				}
				continue // anonymous volume
			}
			if !declared[parts[0]] {
				c.fail(fmt.Sprintf("service %s: %q mounts a host path or an undeclared volume; use a named volume", name, parts[0]), at...)
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			switch typ {
			case "tmpfs":
			case "volume", "":
				src, _ := v["source"].(string)
				if src != "" && !declared[src] {
					c.fail(fmt.Sprintf("service %s: volume %q is not declared in this file", name, src), at...)
				}
			default:
				c.fail(fmt.Sprintf("service %s: %s mounts are not allowed; use a named volume", name, typ), at...)
			}
		default:
			c.fail(fmt.Sprintf("service %s: unreadable volume entry", name), at...)
		}
	}
}

// Volumes are replaced by agent-owned definitions, so only the name is user-chosen.
func (c *checker) checkVolumes(raw any) map[string]bool {
	out := map[string]bool{}
	vols, _ := raw.(map[string]any)
	for name, def := range vols {
		if !volumeNameRE.MatchString(name) {
			c.fail(fmt.Sprintf("volume name %q is not allowed", name), "volumes", name)
			continue
		}
		if m, ok := def.(map[string]any); ok {
			for key := range m {
				if key != "labels" {
					c.fail(fmt.Sprintf("volume %s: %q is not allowed (Ownship manages where volumes live)", name, key), "volumes", name)
				}
			}
		} else if def != nil {
			c.fail(fmt.Sprintf("volume %s must be empty or a mapping", name), "volumes", name)
		}
		out[name] = true
	}
	return out
}

func (c *checker) checkNetworks(raw any) map[string]bool {
	out := map[string]bool{}
	nets, _ := raw.(map[string]any)
	for name, def := range nets {
		if name == linkNetworkKey {
			c.fail(fmt.Sprintf("network name %q is reserved", name), "networks", name)
			continue
		}
		if m, ok := def.(map[string]any); ok {
			for key, val := range m {
				switch key {
				case "internal", "labels", "attachable", "ipam", "enable_ipv6":
				case "driver":
					if val != "bridge" {
						c.fail(fmt.Sprintf("network %s: only the bridge driver is allowed", name), "networks", name)
					}
				default:
					c.fail(fmt.Sprintf("network %s: %q is not allowed", name, key), "networks", name)
				}
			}
		}
		out[name] = true
	}
	return out
}

// configs/secrets may be inline or from env, never a host file.
func (c *checker) checkFileObjects(raw any, kind string) map[string]bool {
	out := map[string]bool{}
	objs, _ := raw.(map[string]any)
	for name, def := range objs {
		m, _ := def.(map[string]any)
		_, hasContent := m["content"]
		_, hasEnv := m["environment"]
		if !hasContent && !hasEnv {
			c.fail(fmt.Sprintf("%s %s: only content or environment sources are allowed", kind, name), kind, name)
		}
		for key := range m {
			if key != "content" && key != "environment" && key != "labels" {
				c.fail(fmt.Sprintf("%s %s: %q is not allowed", kind, name, key), kind, name)
			}
		}
		out[name] = true
	}
	return out
}

func augment(doc map[string]any, o Options) []string {
	var notes []string
	services := doc["services"].(map[string]any)
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := services[name].(map[string]any)
		// Published ports would bypass the ingress; fixed names collide across workloads.
		if _, ok := svc["ports"]; ok {
			delete(svc, "ports")
			notes = append(notes, fmt.Sprintf("service %s: ports removed; Ownship routes to the primary service and other services stay on the project network", name))
		}
		if _, ok := svc["container_name"]; ok {
			delete(svc, "container_name")
			notes = append(notes, fmt.Sprintf("service %s: container_name removed; Ownship names the containers", name))
		}
		labels := labelMap(svc["labels"])
		labels["ownship.workload"] = o.WorkloadID
		svc["labels"] = labels
		if _, ok := svc["restart"]; !ok {
			svc["restart"] = "unless-stopped"
			if o.Restart != "" {
				svc["restart"] = o.Restart
			}
		}
		if name != o.PrimaryService {
			continue
		}
		svc["container_name"] = o.PrimaryContainer
		if o.PrimaryPublish != "" {
			svc["ports"] = []any{o.PrimaryPublish}
		}
		if o.EnvFile != "" {
			svc["env_file"] = []any{o.EnvFile}
		}
		if o.LinkNetwork != "" {
			svc["networks"] = withNetwork(svc["networks"], linkNetworkKey)
		}
		if g := o.GPU; g != nil {
			devices := make([]any, 0, len(g.Devices))
			for _, d := range g.Devices {
				devices = append(devices, d)
			}
			if len(devices) > 0 {
				svc["devices"] = devices
			}
			if g.GroupVideo {
				svc["group_add"] = []any{"video"}
			}
			if g.Nvidia {
				deploy, _ := svc["deploy"].(map[string]any)
				if deploy == nil {
					deploy = map[string]any{}
				}
				res, _ := deploy["resources"].(map[string]any)
				if res == nil {
					res = map[string]any{}
				}
				reservations, _ := res["reservations"].(map[string]any)
				if reservations == nil {
					reservations = map[string]any{}
				}
				reservations["devices"] = []any{map[string]any{"driver": "nvidia", "count": "all", "capabilities": []any{"gpu"}}}
				res["reservations"], deploy["resources"], svc["deploy"] = reservations, res, deploy
			}
		}
	}
	if o.LinkNetwork != "" {
		nets, _ := doc["networks"].(map[string]any)
		if nets == nil {
			nets = map[string]any{}
		}
		nets[linkNetworkKey] = map[string]any{"external": true, "name": o.LinkNetwork}
		doc["networks"] = nets
	}
	if vols, ok := doc["volumes"].(map[string]any); ok && o.Volume != nil {
		for name := range vols {
			vols[name] = o.Volume(name)
		}
	}
	for key := range doc {
		if key == "version" || strings.HasPrefix(key, "x-") {
			delete(doc, key)
		}
	}
	return notes
}

func withNetwork(raw any, extra string) any {
	switch n := raw.(type) {
	case []any:
		return append(n, extra)
	case map[string]any:
		n[extra] = nil
		return n
	default:
		return []any{"default", extra}
	}
}

func interpolateAll(v any, env map[string]string) (any, error) {
	switch t := v.(type) {
	case string:
		return interpolate(t, env)
	case []any:
		for i := range t {
			r, err := interpolateAll(t[i], env)
			if err != nil {
				return nil, err
			}
			t[i] = r
		}
		return t, nil
	case map[string]any:
		for k, val := range t {
			r, err := interpolateAll(val, env)
			if err != nil {
				return nil, err
			}
			t[k] = r
		}
		return t, nil
	case map[any]any:
		// Non-string keys would slip past every check below.
		return nil, fmt.Errorf("mapping keys must be strings")
	default:
		return v, nil
	}
}

// escapeDollars stops compose interpolating already-resolved values again.
func escapeDollars(v any) any {
	switch t := v.(type) {
	case string:
		return strings.ReplaceAll(t, "$", "$$")
	case []any:
		for i := range t {
			t[i] = escapeDollars(t[i])
		}
		return t
	case map[string]any:
		for k, val := range t {
			t[k] = escapeDollars(val)
		}
		return t
	default:
		return v
	}
}

func safeRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "://") || strings.Contains(p, "\\") {
		return false
	}
	clean := path.Clean(p)
	return clean != ".." && !strings.HasPrefix(clean, "../")
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func labelKeys(v any) map[string]bool {
	out := map[string]bool{}
	for k := range labelMap(v) {
		out[k] = true
	}
	return out
}

func labelMap(v any) map[string]any {
	out := map[string]any{}
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			out[k] = val
		}
	case []any:
		for _, item := range t {
			k, val, _ := strings.Cut(fmt.Sprint(item), "=")
			out[k] = val
		}
	}
	return out
}

func networkNames(v any) []string {
	switch t := v.(type) {
	case []any:
		return stringList(t)
	case map[string]any:
		out := make([]string, 0, len(t))
		for k := range t {
			out = append(out, k)
		}
		return out
	}
	return nil
}

func objectRefs(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, item := range list {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			src, _ := t["source"].(string)
			out = append(out, src)
		}
	}
	return out
}

// lineOf finds a key's line in the original file, for error reports.
func lineOf(root *yaml.Node, keys ...string) int {
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	line := 0
	for _, key := range keys {
		if n.Kind != yaml.MappingNode {
			return line
		}
		found := false
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				line = n.Content[i].Line
				n = n.Content[i+1]
				found = true
				break
			}
		}
		if !found {
			return line
		}
	}
	return line
}

func set(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, i := range items {
		out[i] = true
	}
	return out
}
