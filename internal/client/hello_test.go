package client

import (
	"slices"
	"testing"

	"github.com/ownship/agent/internal/config"
	"github.com/ownship/agent/internal/executors"
	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
	"github.com/ownship/agent/internal/router"
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

func TestHelloAnnouncesVMsOnlyWhenCapable(t *testing.T) {
	img := &agentv1.VmImage{Id: "debian-12", Usable: true}
	capable := hello(config.Config{}, nil, nil, nil, "", nil, "", nil).GetHello()
	withVMs(capable, executors.VMReport{Capable: true, Images: []*agentv1.VmImage{img}})
	if !slices.Contains(capable.GetCapabilities(), VMImageFetchCapability) {
		t.Fatal("a capable host fetches catalogue images")
	}
	if !slices.Contains(capable.GetCapabilities(), GuestVMCapability) || len(capable.GetVmImages()) != 1 {
		t.Fatalf("capable host: %v %v", capable.GetCapabilities(), capable.GetVmImages())
	}
	if slices.Contains(router.Capabilities, GuestVMCapability) {
		t.Fatal("announcing guest_vm must not leak into the shared capability list")
	}

	blocked := hello(config.Config{}, nil, nil, nil, "", nil, "", nil).GetHello()
	withVMs(blocked, executors.VMReport{BlockedReason: "VMs need an x86_64 host.", Images: []*agentv1.VmImage{img}})
	if slices.Contains(blocked.GetCapabilities(), VMImageFetchCapability) {
		t.Fatal("a blocked host must not be sent catalogue images")
	}
	if slices.Contains(blocked.GetCapabilities(), GuestVMCapability) || blocked.GetVmBlockedReason() == "" {
		t.Fatalf("blocked host: %v %q", blocked.GetCapabilities(), blocked.GetVmBlockedReason())
	}
}

// Going back to an LXC needs no VM capability, so guest_convert stands on its own.
func TestHelloAnnouncesConvertApartFromVMs(t *testing.T) {
	h := hello(config.Config{}, nil, nil, nil, "", nil, "", nil).GetHello()
	withVMs(h, executors.VMReport{Convert: true, BlockedReason: "No /dev/kvm on this host."})
	if !slices.Contains(h.GetCapabilities(), GuestConvertCapability) || slices.Contains(h.GetCapabilities(), GuestVMCapability) {
		t.Fatalf("capabilities %v", h.GetCapabilities())
	}
	docker := hello(config.Config{}, nil, nil, nil, "", nil, "", nil).GetHello()
	withVMs(docker, executors.VMReport{})
	if slices.Contains(docker.GetCapabilities(), GuestConvertCapability) {
		t.Fatal("a Docker host must not announce guest_convert")
	}
}
