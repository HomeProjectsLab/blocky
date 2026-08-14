package tui

import (
	"os"
	"strconv"
	"strings"
)

// Vitals is the Pi's own health, read from /proc and /sys. Any field that can't
// be read stays at its zero value and Has* reports false (rendered "N/A").
type Vitals struct {
	Load    float64 // 1-min load average
	HasLoad bool

	MemUsedFrac float64 // 0..1
	MemTotalKB  int64
	HasMem      bool

	TempC   float64
	HasTemp bool
}

// ReadVitals samples /proc/loadavg, /proc/meminfo and the thermal zone. Safe on
// non-Linux: the files are absent, so every field degrades to unavailable.
func ReadVitals() Vitals {
	var v Vitals

	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if load, ok := parseLoadavg(string(b)); ok {
			v.Load, v.HasLoad = load, true
		}
	}

	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		if frac, total, ok := parseMeminfo(string(b)); ok {
			v.MemUsedFrac, v.MemTotalKB, v.HasMem = frac, total, true
		}
	}

	if b, err := os.ReadFile("/sys/class/thermal/thermal_zone0/temp"); err == nil {
		if c, ok := parseTemp(string(b)); ok {
			v.TempC, v.HasTemp = c, true
		}
	}

	return v
}

func parseLoadavg(s string) (float64, bool) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0, false
	}

	load, err := strconv.ParseFloat(f[0], 64)

	return load, err == nil
}

// parseMeminfo returns used fraction (1 - MemAvailable/MemTotal) and total KB.
func parseMeminfo(s string) (usedFrac float64, totalKB int64, ok bool) {
	var total, avail int64

	for line := range strings.SplitSeq(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}

		val, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = val
		case "MemAvailable:":
			avail = val
		}
	}

	if total <= 0 {
		return 0, 0, false
	}

	return float64(total-avail) / float64(total), total, true
}

// parseTemp converts a thermal-zone millidegree reading to Celsius.
func parseTemp(s string) (float64, bool) {
	milli, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, false
	}

	return float64(milli) / 1000, true
}
