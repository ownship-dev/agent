package executors

import (
	"os"
	"reflect"
	"regexp"
	"testing"
)

// RunSpec fields one orchestrator deliberately ignores. Any other field read by only one
// orchestrator fails the test.
var onlyOneOrchestrator = map[string]string{
	"TargetNode":       "Proxmox cluster node; a Docker host has no nodes",
	"BaseTemplate":     "LXC base template; LocalDocker runs images directly",
	"TemplateVmid":     "golden CT template; Proxmox only",
	"BaseOS":           "LXC base OS catalogue; Proxmox only",
	"Network":          "LXC net0 (static/DHCP); LocalDocker has no per-workload network",
	"LxcPassword":      "root password of the LXC itself; Docker has no LXC layer",
	"Repo":             "Proxmox builds inside the LXC; LocalDocker builds on the host (Deployer, HostBuilds)",
	"Cache":            "in-LXC checkout cache; LocalDocker's build cache lives in the Deployer",
	"Build":            "build type for the in-LXC build; LocalDocker's build happens in the Deployer",
	"RegistryAuth":     "LocalDocker logs in on the host before Run (deployer.go, HostBuilds)",
	"VmImage":          "image a Proxmox VM boots from; LocalDocker refuses VM workloads outright",
	"VmImageSource":    "where a Proxmox VM's catalogue image is fetched from; VMs are Proxmox only",
	"RhelSubscription": "Red Hat registration of a RHEL VM; VMs are Proxmox only",
}

func TestRunSpecFieldsHaveOrchestratorParity(t *testing.T) {
	local, err := os.ReadFile("local.go")
	if err != nil {
		t.Fatal(err)
	}
	proxmox, err := os.ReadFile("proxmox.go")
	if err != nil {
		t.Fatal(err)
	}
	uses := func(src []byte, field string) bool {
		return regexp.MustCompile(`\bspec\.` + field + `\b`).Match(src)
	}

	seen := map[string]bool{}
	rt := reflect.TypeOf(RunSpec{})
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i).Name
		seen[field] = true
		inLocal, inProxmox := uses(local, field), uses(proxmox, field)
		if inLocal == inProxmox {
			continue
		}
		if _, ok := onlyOneOrchestrator[field]; !ok {
			t.Errorf("RunSpec.%s is read by only one orchestrator (local=%v proxmox=%v); handle it in both "+
				"(ProxmoxLXC's reused-LXC path too) or list it in onlyOneOrchestrator with a reason",
				field, inLocal, inProxmox)
		}
	}
	for field := range onlyOneOrchestrator {
		if !seen[field] {
			t.Errorf("onlyOneOrchestrator lists %s, which RunSpec no longer has", field)
		}
	}
}
