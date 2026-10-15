// Package hardware reports SMART, temperatures, UPS (NUT) and power. Each source is optional.
package hardware

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/ownship/agent/internal/gen/ownship/agent/v1"
)

const commandTimeout = 20 * time.Second

// Collector keeps the previous RAPL reading, since power is energy over time.
type Collector struct {
	// Swappable for tests.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	Sys string

	mu         sync.Mutex
	prevEnergy map[string]uint64
	prevAt     time.Time
}

func New() *Collector {
	return &Collector{
		Sys: "/sys",
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, commandTimeout)
			defer cancel()
			// smartctl's exit code is a bitmask, even with valid JSON.
			out, err := exec.CommandContext(ctx, name, args...).Output()
			if len(out) > 0 {
				return out, nil
			}
			return out, err
		},
	}
}

func (c *Collector) Report(ctx context.Context, node string) *agentv1.HardwareReport {
	r := &agentv1.HardwareReport{
		Node:         node,
		Disks:        c.disks(ctx),
		Temperatures: c.temperatures(),
		Ups:          c.ups(ctx),
	}
	r.PowerWatts = c.raplWatts()
	if r.PowerWatts == 0 && r.Ups != nil {
		r.PowerWatts = c.upsWatts(ctx, r.Ups)
	}
	return r
}

type smartScan struct {
	Devices []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"devices"`
}

type smartInfo struct {
	ModelName   string `json:"model_name"`
	SmartStatus *struct {
		Passed bool `json:"passed"`
	} `json:"smart_status"`
	Temperature struct {
		Current float64 `json:"current"`
	} `json:"temperature"`
	PowerOnTime struct {
		Hours uint64 `json:"hours"`
	} `json:"power_on_time"`
	ATA struct {
		Table []struct {
			ID  int `json:"id"`
			Raw struct {
				Value uint64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMe *struct {
		PercentageUsed uint32 `json:"percentage_used"`
	} `json:"nvme_smart_health_information_log"`
}

func (c *Collector) disks(ctx context.Context) []*agentv1.DiskHealth {
	out, err := c.Run(ctx, "smartctl", "--scan", "-j")
	if err != nil {
		return nil
	}
	var scan smartScan
	if json.Unmarshal(out, &scan) != nil {
		return nil
	}
	var disks []*agentv1.DiskHealth
	for _, d := range scan.Devices {
		args := []string{"-j", "-H", "-A", "-i"}
		if d.Type != "" {
			args = append(args, "-d", d.Type)
		}
		raw, err := c.Run(ctx, "smartctl", append(args, d.Name)...)
		if err != nil {
			continue
		}
		var info smartInfo
		if json.Unmarshal(raw, &info) != nil || info.SmartStatus == nil {
			continue // no SMART support, e.g. a virtual disk
		}
		disk := &agentv1.DiskHealth{
			Device:       d.Name,
			Model:        info.ModelName,
			SmartPassed:  info.SmartStatus.Passed,
			TemperatureC: info.Temperature.Current,
			PowerOnHours: info.PowerOnTime.Hours,
		}
		for _, a := range info.ATA.Table {
			if a.ID == 5 { // Reallocated_Sector_Ct
				disk.ReallocatedSectors = a.Raw.Value
			}
		}
		if info.NVMe != nil {
			disk.PercentageUsed = info.NVMe.PercentageUsed
		}
		disks = append(disks, disk)
	}
	return disks
}

// temperatures reads every hwmon sensor (millidegrees in temp*_input).
func (c *Collector) temperatures() []*agentv1.Temperature {
	inputs, _ := filepath.Glob(filepath.Join(c.Sys, "class/hwmon/hwmon*/temp*_input"))
	sort.Strings(inputs)
	var out []*agentv1.Temperature
	for _, in := range inputs {
		milli, err := readInt(in)
		if err != nil || milli <= 0 {
			continue
		}
		dir := filepath.Dir(in)
		sensor := readTrim(filepath.Join(dir, "name"))
		label := readTrim(strings.TrimSuffix(in, "_input") + "_label")
		if label == "" {
			label = strings.TrimSuffix(filepath.Base(in), "_input")
		}
		out = append(out, &agentv1.Temperature{Sensor: sensor, Label: label, Celsius: float64(milli) / 1000})
	}
	return out
}

// ups reads the first UPS that NUT knows about.
func (c *Collector) ups(ctx context.Context) *agentv1.UpsStatus {
	list, err := c.Run(ctx, "upsc", "-l")
	if err != nil {
		return nil
	}
	names := strings.Fields(string(list))
	if len(names) == 0 {
		return nil
	}
	vars := c.upsVars(ctx, names[0])
	if vars == nil {
		return nil
	}
	runtime, _ := strconv.ParseFloat(vars["battery.runtime"], 64)
	charge, _ := strconv.ParseFloat(vars["battery.charge"], 64)
	load, _ := strconv.ParseFloat(vars["ups.load"], 64)
	return &agentv1.UpsStatus{
		Name:           names[0],
		Status:         vars["ups.status"],
		ChargePct:      charge,
		RuntimeSeconds: uint32(runtime),
		LoadPct:        load,
	}
}

func (c *Collector) upsVars(ctx context.Context, name string) map[string]string {
	out, err := c.Run(ctx, "upsc", name)
	if err != nil {
		return nil
	}
	vars := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if ok {
			vars[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return vars
}

func (c *Collector) upsWatts(ctx context.Context, u *agentv1.UpsStatus) float64 {
	vars := c.upsVars(ctx, u.GetName())
	if w, err := strconv.ParseFloat(vars["ups.realpower"], 64); err == nil && w > 0 {
		return w
	}
	nominal, _ := strconv.ParseFloat(vars["ups.realpower.nominal"], 64)
	if nominal == 0 {
		return 0
	}
	return nominal * u.GetLoadPct() / 100
}

// raplWatts is package power averaged since the last call; the first call only primes it.
func (c *Collector) raplWatts() float64 {
	zones, _ := filepath.Glob(filepath.Join(c.Sys, "class/powercap/intel-rapl:[0-9]*"))
	now := time.Now()
	energy := map[string]uint64{}
	for _, z := range zones {
		if strings.Count(filepath.Base(z), ":") != 1 {
			continue // sub-zones are already counted in the package
		}
		if v, err := readInt(filepath.Join(z, "energy_uj")); err == nil {
			energy[z] = uint64(v)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, prevAt := c.prevEnergy, c.prevAt
	c.prevEnergy, c.prevAt = energy, now
	if len(energy) == 0 || prev == nil {
		return 0
	}
	secs := now.Sub(prevAt).Seconds()
	if secs <= 0 {
		return 0
	}
	var joules float64
	for z, e := range energy {
		p, ok := prev[z]
		if !ok {
			continue
		}
		if e < p { // the counter wrapped
			limit, err := readInt(filepath.Join(z, "max_energy_range_uj"))
			if err != nil {
				continue
			}
			e += uint64(limit)
		}
		joules += float64(e-p) / 1e6
	}
	return joules / secs
}

func readInt(path string) (int64, error) {
	return strconv.ParseInt(readTrim(path), 10, 64)
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
