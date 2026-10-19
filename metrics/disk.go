package metrics

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type DiskCapacity struct {
	Device  string  `json:"device"`
	Mount   string  `json:"mount"`
	TotalGB float64 `json:"total_gb"`
	UsedGB  float64 `json:"used_gb"`
	FreeGB  float64 `json:"free_gb"`
	UsedPct float64 `json:"used_pct"`
}

func ReadDiskCapacity() ([]DiskCapacity, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := make(map[string]bool)
	var result []DiskCapacity

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		dev := fields[0]
		mount := fields[1]
		fstype := fields[2]

		if !strings.HasPrefix(dev, "/dev/") {
			continue
		}
		// skip EFI and boot partitions
		if strings.HasPrefix(mount, "/boot") {
			continue
		}
		// skip snap loop mounts (read-only squashfs images, always 100% used)
		if fstype == "squashfs" {
			continue
		}

		base := filepath.Base(dev)
		if seen[base] {
			continue
		}
		seen[base] = true

		var stat syscall.Statfs_t
		if err := syscall.Statfs(mount, &stat); err != nil {
			continue
		}
		if stat.Blocks == 0 {
			continue
		}

		bs := float64(stat.Bsize)
		totalGB := float64(stat.Blocks) * bs / 1e9
		freeGB := float64(stat.Bfree) * bs / 1e9
		usedGB := totalGB - freeGB
		usedPct := 100 * float64(stat.Blocks-stat.Bfree) / float64(stat.Blocks)

		// skip very small pseudo-mounts (< 100 MB)
		if totalGB < 0.1 {
			continue
		}

		result = append(result, DiskCapacity{
			Device:  base,
			Mount:   mount,
			TotalGB: totalGB,
			UsedGB:  usedGB,
			FreeGB:  freeGB,
			UsedPct: usedPct,
		})
	}
	return result, scanner.Err()
}
