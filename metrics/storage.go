package metrics

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// StorageCategory is one slice of the root filesystem's used space, grouped by
// what the space is for rather than by where it sits. The VPS spreads each
// concern across several roots — deployed apps in /opt, the Coder dev container
// (and every codebase inside it) in /var/lib/docker, the OS in /usr — so a plain
// directory tree would not line up with how you actually think about the disk.
type StorageCategory struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// StorageBreakdown is the root filesystem's capacity plus where its used space
// goes. Categories are largest-first; Other is used space no category claimed.
// Categories + Other + Free always sum to Total.
type StorageBreakdown struct {
	TotalBytes int64             `json:"total_bytes"`
	UsedBytes  int64             `json:"used_bytes"`
	FreeBytes  int64             `json:"free_bytes"`
	OtherBytes int64             `json:"other_bytes"`
	Categories []StorageCategory `json:"categories"`
}

// storageCategories maps a category to the paths that make it up. Every path must
// live on the root filesystem — du runs with -x, which keeps the scan on one
// filesystem, so a tmpfs or bind mount under one of these is not double-counted.
var storageCategories = []struct {
	name  string
	paths []string
}{
	// The Coder container is the biggest thing on the VPS and holds every codebase
	// and dev tool; containerd sits next to it with the image layers.
	{"Workspace & containers", []string{"/var/lib/docker", "/var/lib/containerd"}},
	// Everything deployed from GitHub, plus each app's embedded H2 DBs and media.
	{"Applications", []string{"/opt"}},
	{"System & OS", []string{"/usr", "/var/lib/snapd", "/var/cache", "/var/lib/apt", "/boot"}},
	{"Admin & scratch", []string{"/root"}},
	{"Databases", []string{"/var/lib/postgresql"}},
	{"Logs", []string{"/var/log"}},
}

// ReadStorageBreakdown measures the root filesystem's capacity and du's each
// category's paths. It shells out to du rather than walking the tree in Go: du is
// far faster over the container's overlay storage (millions of small files), and
// this only runs on a slow timer, never on the per-second path.
func ReadStorageBreakdown() (StorageBreakdown, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return StorageBreakdown{}, err
	}
	bs := int64(st.Bsize)
	total := int64(st.Blocks) * bs
	free := int64(st.Bfree) * bs // matches the capacity meter (free, not avail)
	used := total - free

	// One du call for every path: each is scanned once and printed on its own line.
	// --block-size=1 reports the actual bytes on disk (not apparent size); -x keeps
	// the scan on the root filesystem; -s summarises each argument.
	args := []string{"-sx", "--block-size=1"}
	pathCat := make(map[string]string)
	for _, c := range storageCategories {
		for _, p := range c.paths {
			args = append(args, p)
			pathCat[p] = c.name
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// A missing path (du exits non-zero) still leaves the good lines on stdout, so
	// ignore the error and parse whatever came back.
	out, _ := exec.CommandContext(ctx, "du", args...).Output()

	sums := make(map[string]int64)
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		// "<bytes>\t<path>" — the paths here contain no whitespace.
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if cat, ok := pathCat[fields[len(fields)-1]]; ok {
			sums[cat] += n
		}
	}

	var cats []StorageCategory
	var claimed int64
	for _, c := range storageCategories {
		if b := sums[c.name]; b > 0 {
			cats = append(cats, StorageCategory{Name: c.name, Bytes: b})
			claimed += b
		}
	}
	sort.SliceStable(cats, func(i, j int) bool { return cats[i].Bytes > cats[j].Bytes })

	other := used - claimed
	if other < 0 {
		other = 0
	}

	return StorageBreakdown{
		TotalBytes: total,
		UsedBytes:  used,
		FreeBytes:  free,
		OtherBytes: other,
		Categories: cats,
	}, nil
}
