package metrics

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type CPUSample struct {
	Total uint64
	Idle  uint64
}

type CPUReading struct {
	Overall CPUSample
	Cores   []CPUSample
}

func ReadCPU() (CPUReading, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return CPUReading{}, err
	}
	defer f.Close()

	var r CPUReading
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu") {
			break
		}
		s, err := parseCPULine(line)
		if err != nil {
			continue
		}
		if line[:4] == "cpu " {
			r.Overall = s
		} else {
			r.Cores = append(r.Cores, s)
		}
	}
	return r, scanner.Err()
}

func parseCPULine(line string) (CPUSample, error) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return CPUSample{}, fmt.Errorf("short cpu line")
	}
	var vals [10]uint64
	for i := 1; i < len(fields) && i <= 10; i++ {
		v, _ := strconv.ParseUint(fields[i], 10, 64)
		vals[i-1] = v
	}
	// idle = idle + iowait (index 3 and 4)
	idle := vals[3] + vals[4]
	total := vals[0] + vals[1] + vals[2] + vals[3] + vals[4] + vals[5] + vals[6] + vals[7]
	return CPUSample{Total: total, Idle: idle}, nil
}

// CPUShareOfVPS turns two readings of one cgroup's cumulative CPU time into the
// share of the whole VPS it burned in between, 0-100 across every core.
//
// Percent of all cores, not of one core: this number is drawn on the same page as
// the overall CPU figure, and a service reading "180%" beside a VPS reading "45%"
// is a puzzle rather than a measurement. On this basis the services add up to
// roughly the total, which is the entire point of showing them — attribution.
//
// The kernel only offers a counter that climbs since the cgroup was created, so
// "right now" has to be a difference between two polls. The first poll after
// startup therefore has nothing to subtract from and reports 0, not a spike.
func CPUShareOfVPS(prev, curr uint64, elapsed time.Duration) float64 {
	// A restarted unit gets a fresh cgroup whose counter begins again at zero, so
	// the new reading is *below* the old one. That is a counter reset, not idleness
	// and not a negative rate: report nothing until the next poll establishes a
	// baseline, rather than drawing a garbage bar.
	if elapsed <= 0 || curr <= prev {
		return 0
	}
	capacity := float64(runtime.NumCPU()) * float64(elapsed.Nanoseconds())
	if capacity <= 0 {
		return 0
	}
	pct := 100 * float64(curr-prev) / capacity
	if pct > 100 {
		return 100
	}
	return pct
}

func CPUPercent(prev, curr CPUSample) float64 {
	td := float64(curr.Total - prev.Total)
	if td <= 0 {
		return 0
	}
	id := float64(curr.Idle - prev.Idle)
	pct := 100 * (1 - id/td)
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
