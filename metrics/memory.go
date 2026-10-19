package metrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type MemSnapshot struct {
	TotalMB     float64
	UsedMB      float64
	CachedMB    float64
	BuffersMB   float64
	FreeMB      float64
	SwapTotalMB float64
	SwapUsedMB  float64
}

func ReadMemory() (MemSnapshot, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemSnapshot{}, err
	}
	defer f.Close()

	kv := make(map[string]uint64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		valStr := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(parts[1]), "kB"))
		val, _ := strconv.ParseUint(strings.TrimSpace(valStr), 10, 64)
		kv[key] = val
	}

	toMB := func(kb uint64) float64 { return float64(kb) / 1024 }

	total := kv["MemTotal"]
	free := kv["MemFree"]
	buffers := kv["Buffers"]
	cached := kv["Cached"] + kv["SReclaimable"] - kv["Shmem"]
	used := total - free - buffers - cached
	swapTotal := kv["SwapTotal"]
	swapFree := kv["SwapFree"]

	return MemSnapshot{
		TotalMB:     toMB(total),
		UsedMB:      toMB(used),
		CachedMB:    toMB(cached),
		BuffersMB:   toMB(buffers),
		FreeMB:      toMB(free),
		SwapTotalMB: toMB(swapTotal),
		SwapUsedMB:  toMB(swapTotal - swapFree),
	}, scanner.Err()
}
