package metrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// HostInfo is the VPS itself: who it is and how hard it is working. It answers
// the questions a chart cannot ("has it rebooted?", "is it overloaded?").
type HostInfo struct {
	Hostname  string  `json:"hostname"`
	Kernel    string  `json:"kernel"`
	UptimeSec float64 `json:"uptime_sec"`
	Load1     float64 `json:"load1"`
	Load5     float64 `json:"load5"`
	Load15    float64 `json:"load15"`
	Cores     int     `json:"cores"`
}

// ReadHost never fails hard: a missing field is worth less than a blank page,
// so anything unreadable is simply left at its zero value.
func ReadHost(cores int) HostInfo {
	h := HostInfo{Cores: cores}

	if name, err := os.Hostname(); err == nil {
		h.Hostname = name
	}
	h.Kernel = kernelRelease()
	h.UptimeSec = uptimeSeconds()
	h.Load1, h.Load5, h.Load15 = loadAverage()

	return h
}

func kernelRelease() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	return charsToString(u.Release[:])
}

// Utsname fields are fixed-size C char arrays; take everything up to the NUL.
func charsToString(ca []int8) string {
	b := make([]byte, 0, len(ca))
	for _, c := range ca {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

// uptimeSeconds is also the clock that systemd's *Monotonic timestamps are
// measured against, which is how a unit's uptime is derived.
func uptimeSeconds() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

func loadAverage() (float64, float64, float64) {
	f, err := os.Open("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		return 0, 0, 0
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 3 {
		return 0, 0, 0
	}
	l1, _ := strconv.ParseFloat(fields[0], 64)
	l5, _ := strconv.ParseFloat(fields[1], 64)
	l15, _ := strconv.ParseFloat(fields[2], 64)
	return l1, l5, l15
}
