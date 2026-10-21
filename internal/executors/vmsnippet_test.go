package executors

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

func fakeStorage(t *testing.T, cfg string) (dir string, set *[]string) {
	t.Helper()
	dir = t.TempDir()
	var calls []string
	oc, os_ := storageConfig, setStorageContent
	t.Cleanup(func() { storageConfig, setStorageContent = oc, os_ })
	storageConfig = func(_ context.Context, storage string) ([]byte, error) {
		return []byte(strings.ReplaceAll(cfg, "DIR", dir)), nil
	}
	setStorageContent = func(_ context.Context, storage, content string) ([]byte, error) {
		calls = append(calls, storage+" "+content)
		return nil, nil
	}
	return dir, &calls
}

func TestVendorSnippetEnablesSnippetsOnceAndWritesTheFile(t *testing.T) {
	o := newTestProxmox(t)
	dir, set := fakeStorage(t, `{"type":"dir","path":"DIR","content":"iso,vztmpl,backup"}`)
	ref, err := o.ensureVendorSnippet(context.Background())
	if err != nil || ref != "local:snippets/ownship-vendor.yaml" {
		t.Fatalf("%q %v", ref, err)
	}
	if len(*set) != 1 || (*set)[0] != "local iso,vztmpl,backup,snippets" {
		t.Fatalf("content kept and snippets added: %v", *set)
	}
	data, err := os.ReadFile(filepath.Join(dir, "snippets", "ownship-vendor.yaml"))
	if err != nil || string(data) != vendorSnippet {
		t.Fatalf("snippet: %q %v", data, err)
	}
}

func TestVendorSnippetLeavesStorageAloneWhenItHasSnippets(t *testing.T) {
	o := newTestProxmox(t)
	_, set := fakeStorage(t, `{"type":"dir","path":"DIR","content":"snippets,iso"}`)
	if _, err := o.ensureVendorSnippet(context.Background()); err != nil || len(*set) != 0 {
		t.Fatalf("%v %v", *set, err)
	}
}

func TestVendorSnippetRefusesANonDirectoryStorage(t *testing.T) {
	o := newTestProxmox(t)
	fakeStorage(t, `{"type":"lvmthin","content":"images"}`)
	if _, err := o.ensureVendorSnippet(context.Background()); err == nil {
		t.Fatal("expected a refusal")
	}
}

func TestRegisterRHELSendsTheKeyOnStdinOnly(t *testing.T) {
	o, _ := testVMGuest(t)
	o.vm.ips["201"] = vmIP{ip: "192.0.2.10", at: time.Now()}
	var argv []string
	var stdin []byte
	orig := vmRunStdin
	vmRunStdin = func(_ context.Context, a []string, in []byte) ([]byte, error) {
		argv, stdin = a, in
		return nil, nil
	}
	t.Cleanup(func() { vmRunStdin = orig })
	sub := &agentv1.RhelSubscription{OrgId: "1234567", ActivationKey: "ownship-key"}
	if err := o.vm.registerRHEL(context.Background(), "201", sub, noLog); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), "ownship-key") {
		t.Fatalf("the key reached argv: %v", argv)
	}
	if string(stdin) != "1234567\nownship-key\n" {
		t.Fatalf("stdin = %q", stdin)
	}
	bad := &agentv1.RhelSubscription{OrgId: "1", ActivationKey: "a\nrm -rf /"}
	if err := o.vm.registerRHEL(context.Background(), "201", bad, noLog); err == nil {
		t.Fatal("a key with a newline must be refused")
	}
}

func TestRHELImageWithoutSubscriptionIsRefusedBeforeQM(t *testing.T) {
	o := cacheProxmox(t)
	o.cfg.Bridge, o.cfg.Storage = "vmbr0", "local-lvm"
	vmCapableHost(t, o)
	body := []byte("rhel")
	installVendor(t, &fakeVendor{image: body})
	if err := o.FetchLinkImage(context.Background(), linkCommand(body), (&statusLog{}).fn); err != nil {
		t.Fatal(err)
	}
	calls := fakeQM(t, func([]string) ([]byte, error) { return nil, nil })
	_, err := o.Run(context.Background(), vmSpec(func(s *RunSpec) { s.VmImage = "rhel-9" }))
	if err == nil || !strings.Contains(err.Error(), "Red Hat subscription") {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) > 0 {
		t.Fatalf("qm ran: %v", *calls)
	}
}
