package executors

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

// fakePctScript records each pct call (and the stdin of an untar), serves TARBYTES for a tar
// out of a guest, and keeps a stopped marker per vmid so status follows shutdown and start.
const fakePctScript = `#!/bin/sh
printf '%s\0' "$@" >> "$FAKE_PCT_LOG"
printf '\035' >> "$FAKE_PCT_LOG"
case "$1" in
  exec)
    case "$*" in
      *" tar czf - "*)
        [ -n "$FAKE_PCT_TARFAIL" ] && { printf '\036' >> "$FAKE_PCT_LOG"; echo "tar: boom" >&2; exit 2; }
        printf 'TARBYTES' ;;
      *" tar xzf - "*) cat >> "$FAKE_PCT_LOG" ;;
      *"ip -4 -o addr show eth0"*) echo "2: eth0    inet 10.0.0.9/24 brd 10.0.0.255" ;;
    esac ;;
  status)
    if [ -e "$FAKE_PCT_LOG.stopped.$2" ]; then echo "status: stopped"; else echo "status: running"; fi ;;
  shutdown|stop) : > "$FAKE_PCT_LOG.stopped.$2" ;;
  start) rm -f "$FAKE_PCT_LOG.stopped.$2" ;;
esac
printf '\036' >> "$FAKE_PCT_LOG"
exit 0
`

func fakePct(t *testing.T) func() []sshCall {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "pct"), []byte(fakePctScript), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(bin, "calls")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_PCT_LOG", log)
	return func() []sshCall {
		data, _ := os.ReadFile(log)
		var calls []sshCall
		for _, rec := range strings.Split(string(data), "\036") {
			if rec == "" {
				continue
			}
			args, stdin, _ := strings.Cut(rec, "\035")
			calls = append(calls, sshCall{argv: strings.Split(strings.TrimSuffix(args, "\x00"), "\x00"), stdin: stdin})
		}
		return calls
	}
}

func hasPct(calls []sshCall, prefix ...string) bool {
	_, ok := findCall(calls, prefix...)
	return ok
}

// lxcToVM is an LXC workload "w" on 142 with a volume, on a host that can build VM 201.
func lxcToVM(t *testing.T, cluster string) (*ProxmoxLXC, func() []sshCall, func() []sshCall, *[][]string) {
	t.Helper()
	fastVMWaits(t)
	o, _ := testVMGuest(t)
	vmCapableHost(t, o)
	vmImageInDir(t, o)
	if err := o.state.set("w", "142"); err != nil {
		t.Fatal(err)
	}
	fakeNextVmid(t, "9000", "201")
	fakeClusterGuests(t, cluster, nil)
	qm := vmQM(t, testHostKey(t), func(args []string) ([]byte, error, bool) {
		if args[0] == "importdisk" {
			out, err := importReply(args)
			return out, err, true
		}
		return nil, nil, false
	})
	fakeSSHProbe(t, func([]string) error { return nil })
	_, ssh, _ := fakeSSH(t)
	pcts := fakePct(t)
	return o, pcts, ssh, qm
}

const bothGuests = `[{"vmid":142,"type":"lxc","tags":"ownship"},{"vmid":201,"type":"qemu","tags":"ownship"}]`

func toVM() ConvertRequest {
	return ConvertRequest{WorkloadID: "w", To: GuestVM, VMImage: "debian-12", Hostname: "app",
		Resources: &agentv1.Resources{Vcpu: 1, MemoryMb: 1024}, Volumes: []string{"data"}, LogLine: noLog}
}

func TestConvertPrepareBuildsTheVMBesideTheLXCAndCopiesVolumes(t *testing.T) {
	o, pcts, ssh, _ := lxcToVM(t, bothGuests)
	ctx := context.Background()

	vmid, err := o.ConvertPrepare(ctx, toVM())
	if err != nil {
		t.Fatal(err)
	}
	if vmid != "201" {
		t.Fatalf("vmid = %q", vmid)
	}
	if p, ok := o.vms.pending("w"); !ok || p != (retiredGuest{Vmid: "201", Kind: "vm"}) {
		t.Fatalf("pending = %+v %v", p, ok)
	}
	if ref, ok := o.guestFor("w"); !ok || ref.vmid != "142" || ref.g.Kind() != GuestLXC {
		t.Fatalf("the LXC must stay the workload's guest until a deploy: %+v", ref)
	}
	calls := pcts()
	if !hasPct(calls, "exec", "142", "--", "docker", "stop", lxcAppContainer) {
		t.Fatal("the app was not stopped before the copy")
	}
	if !hasPct(calls, "exec", "142", "--", "tar", "czf", "-", "-C", "/opt/ownship-volumes/data", ".") {
		t.Fatal("the volume was not read from the LXC")
	}
	if !hasPct(calls, "shutdown", "142") {
		t.Fatal("the LXC was left running beside its replacement")
	}
	untar, ok := findCall(ssh(), "sh", "-c")
	if !ok || !strings.Contains(untar.argv[2], "tar xzf - -C /opt/ownship-volumes/data") || untar.stdin != "TARBYTES" {
		t.Fatalf("untar into the VM = %+v", untar)
	}

	// The GC must keep seeing exactly one workload, and never the pending VM as a stray.
	inv, err := o.List(ctx)
	if err != nil || !inv.Complete || !slices.Equal(inv.IDs, []string{"w"}) {
		t.Fatalf("inventory = %+v %v", inv, err)
	}
	if adopted, _ := o.Adopt(ctx, map[string]string{"other": "201"}); len(adopted) != 0 {
		t.Fatalf("a pending guest was handed to another workload: %v", adopted)
	}
}

func TestConvertDeployPromotesThenCommitDestroysTheLXC(t *testing.T) {
	o, pcts, _, qm := lxcToVM(t, bothGuests)
	ctx := context.Background()
	if _, err := o.ConvertPrepare(ctx, toVM()); err != nil {
		t.Fatal(err)
	}
	clones := countVerb(*qm, "clone")

	ref, err := o.ensureGuest(ctx, vmSpec(func(s *RunSpec) { s.WorkloadID = "w" }))
	if err != nil {
		t.Fatal(err)
	}
	if ref.vmid != "201" || ref.g.Kind() != GuestVM {
		t.Fatalf("deploy ran in %+v", ref)
	}
	if countVerb(*qm, "clone") != clones {
		t.Fatal("the deploy built a second VM instead of promoting the prepared one")
	}
	if r, ok := o.vms.retired("w"); !ok || r != (retiredGuest{Vmid: "142", Kind: "lxc"}) {
		t.Fatalf("retired = %+v %v", r, ok)
	}
	if _, ok := o.vms.pending("w"); ok {
		t.Fatal("pending survived its promotion")
	}
	if _, inLXC := o.state.get("w"); inLXC {
		t.Fatal("the workload is still tracked as an LXC")
	}
	inv, _ := o.List(ctx)
	if !inv.Complete || !slices.Equal(inv.IDs, []string{"w"}) {
		t.Fatalf("inventory = %+v", inv)
	}
	// A replayed Prepare after the promotion answers with the new guest and builds nothing.
	if vmid, err := o.ConvertPrepare(ctx, toVM()); err != nil || vmid != "201" {
		t.Fatalf("replayed prepare = %q %v", vmid, err)
	}

	if err := o.ConvertCommit(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if !hasPct(pcts(), "destroy", "142", "--purge") {
		t.Fatal("commit kept the old LXC")
	}
	if _, ok := o.vms.retired("w"); ok {
		t.Fatal("retired survived the commit")
	}
	before := len(pcts())
	if err := o.ConvertCommit(ctx, "w"); err != nil || len(pcts()) != before {
		t.Fatalf("a replayed commit touched Proxmox: %v", err)
	}
}

func TestConvertRollbackAfterPromotionRestoresTheLXC(t *testing.T) {
	o, pcts, _, qm := lxcToVM(t, bothGuests)
	ctx := context.Background()
	if _, err := o.ConvertPrepare(ctx, toVM()); err != nil {
		t.Fatal(err)
	}
	if _, err := o.ensureGuest(ctx, vmSpec(func(s *RunSpec) { s.WorkloadID = "w" })); err != nil {
		t.Fatal(err)
	}

	if err := o.ConvertRollback(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*qm, func(c []string) bool { return c[0] == "destroy" && c[1] == "201" }) {
		t.Fatalf("the new VM was kept: %v", verbs(*qm))
	}
	ref, ok := o.guestFor("w")
	if !ok || ref.vmid != "142" || ref.g.Kind() != GuestLXC {
		t.Fatalf("workload guest = %+v %v", ref, ok)
	}
	if _, ok := o.vms.get("w"); ok {
		t.Fatal("the VM is still tracked")
	}
	calls := pcts()
	if !hasPct(calls, "start", "142") || !hasPct(calls, "exec", "142", "--", "docker", "start", lxcAppContainer) {
		t.Fatal("the LXC and its app were not started again")
	}
	if _, ok := o.vms.retired("w"); ok {
		t.Fatal("retired survived the rollback")
	}
	if err := o.ConvertRollback(ctx, "w"); err != nil {
		t.Fatalf("a replayed rollback failed: %v", err)
	}
}

func TestConvertPrepareFailureLeavesTheLXCRecoverable(t *testing.T) {
	o, pcts, _, qm := lxcToVM(t, bothGuests)
	t.Setenv("FAKE_PCT_TARFAIL", "1")
	ctx := context.Background()

	if _, err := o.ConvertPrepare(ctx, toVM()); err == nil || !strings.Contains(err.Error(), "tar: boom") {
		t.Fatalf("err = %v", err)
	}
	if countVerb(*qm, "clone") != 0 {
		t.Fatal("a VM was built although the copy failed")
	}
	if _, ok := o.vms.pending("w"); ok {
		t.Fatal("a failed prepare left a pending guest")
	}
	if hasPct(pcts(), "shutdown", "142") {
		t.Fatal("the LXC was shut down before its data was safe")
	}
	if err := o.ConvertRollback(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if !hasPct(pcts(), "exec", "142", "--", "docker", "start", lxcAppContainer) {
		t.Fatal("rollback did not restart the app")
	}
}

func TestConvertRollbackBeforeTheDeployDestroysOnlyThePendingGuest(t *testing.T) {
	o, pcts, _, qm := lxcToVM(t, bothGuests)
	ctx := context.Background()
	if _, err := o.ConvertPrepare(ctx, toVM()); err != nil {
		t.Fatal(err)
	}
	if err := o.ConvertRollback(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*qm, func(c []string) bool { return c[0] == "destroy" && c[1] == "201" }) {
		t.Fatal("the pending VM was kept")
	}
	if hasPct(pcts(), "destroy", "142") {
		t.Fatal("rollback destroyed the original LXC")
	}
	if ref, _ := o.guestFor("w"); ref.vmid != "142" || !hasPct(pcts(), "start", "142") {
		t.Fatalf("the LXC is not back: %+v", ref)
	}
}

func TestConvertCommitLeavesARecycledVmidAlone(t *testing.T) {
	for name, cluster := range map[string]string{
		"now a VM":   `[{"vmid":142,"type":"qemu","tags":"ownship"}]`,
		"not tagged": `[{"vmid":142,"type":"lxc"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			o := newTestProxmox(t)
			fakeClusterGuests(t, cluster, nil)
			pcts := fakePct(t)
			_ = o.vms.set("w", vmWorkload{Vmid: "201"})
			_ = o.vms.setRetired("w", retiredGuest{Vmid: "142", Kind: "lxc"})
			err := o.ConvertCommit(context.Background(), "w")
			if hasPct(pcts(), "destroy", "142") {
				t.Fatal("destroyed a guest that is no longer the retired LXC")
			}
			if name == "not tagged" && err == nil {
				t.Fatal("an untagged LXC on the retired vmid must be reported")
			}
		})
	}
}

func TestConvertVMBackToAnLXC(t *testing.T) {
	fastVMWaits(t)
	o, _ := testVMGuest(t)
	_ = o.vms.set("w", vmWorkload{Vmid: "201"})
	fakeNextVmid(t, "150")
	fakeClusterGuests(t, `[{"vmid":201,"type":"qemu","tags":"ownship"},{"vmid":150,"type":"lxc","tags":"ownship"}]`, nil)
	qm := vmQM(t, testHostKey(t), nil)
	fakeSSHProbe(t, func([]string) error { return nil })
	_, ssh, _ := fakeSSH(t)
	pcts := fakePct(t)
	ctx := context.Background()

	vmid, err := o.ConvertPrepare(ctx, ConvertRequest{WorkloadID: "w", To: GuestLXC, TemplateVmid: "9100",
		Hostname: "app", Volumes: []string{"data"}, LogLine: noLog})
	if err != nil {
		t.Fatal(err)
	}
	if vmid != "150" || !hasPct(pcts(), "clone", "9100", "150", "--hostname", "app") {
		t.Fatalf("vmid %q, pct %v", vmid, pcts())
	}
	if _, ok := findCall(ssh(), "tar", "czf", "-", "-C", "/opt/ownship-volumes/data", "."); !ok {
		t.Fatal("the volume was not read from the VM")
	}
	if !slices.ContainsFunc(*qm, func(c []string) bool { return c[0] == "shutdown" && c[1] == "201" }) {
		t.Fatal("the VM was left running")
	}
	untar, ok := findCall(pcts(), "exec", "150", "--", "sh", "-c")
	if !ok || !strings.Contains(untar.argv[5], "tar xzf - -C /opt/ownship-volumes/data") {
		t.Fatalf("untar into the LXC = %+v", untar)
	}
	if _, inLXC := o.state.get("w"); inLXC {
		t.Fatal("the pending LXC was tracked as the workload's guest")
	}

	ref, err := o.ensureGuest(ctx, RunSpec{WorkloadID: "w", GuestKind: GuestLXC, LogLine: noLog})
	if err != nil || ref.vmid != "150" {
		t.Fatalf("deploy = %+v %v", ref, err)
	}
	if err := o.ConvertCommit(ctx, "w"); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(*qm, func(c []string) bool { return c[0] == "destroy" && c[1] == "201" }) {
		t.Fatal("commit kept the old VM")
	}
}

func TestDestroyRemovesGuestsAConvertKeptAside(t *testing.T) {
	o := newTestProxmox(t)
	fakeClusterGuests(t, `[{"vmid":142,"type":"lxc","tags":"ownship"},{"vmid":150,"type":"lxc","tags":"ownship"}]`, nil)
	pcts := fakePct(t)
	_ = o.state.set("w", "150")
	_ = o.vms.setRetired("w", retiredGuest{Vmid: "142", Kind: "lxc"})
	if err := o.Destroy(context.Background(), "w", false); err != nil {
		t.Fatal(err)
	}
	if !hasPct(pcts(), "destroy", "142", "--purge") || !hasPct(pcts(), "destroy", "150", "--purge") {
		t.Fatalf("pct = %v", pcts())
	}
	if _, ok := o.vms.retired("w"); ok || o.vms.hasWorkloads() {
		t.Fatal("the retired entry outlived its workload")
	}
}

func countVerb(calls [][]string, verb string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 0 && c[0] == verb {
			n++
		}
	}
	return n
}
