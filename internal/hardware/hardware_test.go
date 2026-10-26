package hardware

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeRun(outputs map[string]string) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if out, ok := outputs[key]; ok {
			return []byte(out), nil
		}
		return nil, errors.New("not found: " + key)
	}
}

func TestReport(t *testing.T) {
	sys := t.TempDir()
	write(t, sys, "class/hwmon/hwmon0/name", "coretemp\n")
	write(t, sys, "class/hwmon/hwmon0/temp1_input", "54000\n")
	write(t, sys, "class/hwmon/hwmon0/temp1_label", "Package id 0\n")
	write(t, sys, "class/hwmon/hwmon1/name", "nvme\n")
	write(t, sys, "class/hwmon/hwmon1/temp1_input", "41500\n")

	c := &Collector{Sys: sys, Run: fakeRun(map[string]string{
		"smartctl --scan-open -j": `{"devices":[{"name":"/dev/sda","type":"sat"},{"name":"/dev/nvme0","type":"nvme"},{"name":"/dev/sdz","type":"scsi"}]}`,
		"smartctl -j -H -A -i -d sat /dev/sda": `{"model_name":"WDC RED","smart_status":{"passed":false},"temperature":{"current":38},
			"power_on_time":{"hours":41000},"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":12}},{"id":9,"raw":{"value":41000}}]}}`,
		"smartctl -j -H -A -i -d nvme /dev/nvme0": `{"model_name":"Samsung 980","smart_status":{"passed":true},"temperature":{"current":41},
			"nvme_smart_health_information_log":{"percentage_used":7}}`,
		"smartctl -j -H -A -i -d scsi /dev/sdz": `{"model_name":"virtual"}`,
		"upsc -l":                               "eaton\n",
		"upsc eaton":                            "battery.charge: 87\nbattery.runtime: 1260\nups.load: 23\nups.status: OB DISCHRG\nups.realpower.nominal: 900\n",
	})}

	r := c.Report(context.Background(), "pve1")
	if len(r.Disks) != 2 {
		t.Fatalf("disks = %v (a disk without SMART must be skipped)", r.Disks)
	}
	if d := r.Disks[0]; d.SmartPassed || d.ReallocatedSectors != 12 || d.PowerOnHours != 41000 || d.TemperatureC != 38 {
		t.Fatalf("sata disk = %+v", d)
	}
	if d := r.Disks[1]; !d.SmartPassed || d.PercentageUsed != 7 {
		t.Fatalf("nvme disk = %+v", d)
	}
	if len(r.Temperatures) != 2 || r.Temperatures[0].Celsius != 54 || r.Temperatures[0].Label != "Package id 0" || r.Temperatures[1].Label != "temp1" {
		t.Fatalf("temperatures = %v", r.Temperatures)
	}
	if u := r.Ups; u == nil || u.Status != "OB DISCHRG" || u.RuntimeSeconds != 1260 || u.ChargePct != 87 {
		t.Fatalf("ups = %+v", u)
	}
	// No RAPL here, so power falls back to the UPS: 23% of 900 W.
	if r.PowerWatts < 206.9 || r.PowerWatts > 207.1 {
		t.Fatalf("power = %v", r.PowerWatts)
	}
}

func TestDiskFallsBackToAutodetectedType(t *testing.T) {
	c := &Collector{Sys: t.TempDir(), Run: fakeRun(map[string]string{
		"smartctl --scan-open -j":               `{"devices":[{"name":"/dev/sda","type":"scsi"}]}`,
		"smartctl -j -H -A -i -d scsi /dev/sda": `{"model_name":"ST4000"}`,
		"smartctl -j -H -A -i /dev/sda":         `{"model_name":"ST4000","smart_status":{"passed":true}}`,
	})}
	if d := c.Report(context.Background(), "host").Disks; len(d) != 1 || !d[0].SmartPassed {
		t.Fatalf("disks = %v", d)
	}
}

func TestMissingToolsJustOmitParts(t *testing.T) {
	c := &Collector{Sys: t.TempDir(), Run: fakeRun(nil)}
	r := c.Report(context.Background(), "host")
	if r.Disks != nil || r.Temperatures != nil || r.Ups != nil || r.PowerWatts != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestRAPLPowerIsEnergyOverTime(t *testing.T) {
	sys := t.TempDir()
	write(t, sys, "class/powercap/intel-rapl:0/energy_uj", "1000000")
	write(t, sys, "class/powercap/intel-rapl:0:0/energy_uj", "999999999") // a sub-zone, ignored
	c := &Collector{Sys: sys}
	if w := c.raplWatts(); w != 0 {
		t.Fatalf("first read should only prime, got %v", w)
	}
	c.prevAt = time.Now().Add(-10 * time.Second)
	write(t, sys, "class/powercap/intel-rapl:0/energy_uj", "251000000") // +250 J
	if w := c.raplWatts(); w < 24 || w > 26 {
		t.Fatalf("power = %v, want ~25 W", w)
	}
}
