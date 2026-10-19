package metrics

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

type NetSample struct {
	Iface   string
	RxBytes uint64
	TxBytes uint64
}

type NetMetric struct {
	Iface string  `json:"iface"`
	RxMBs float64 `json:"rx_mbs"`
	TxMBs float64 `json:"tx_mbs"`
}

func ReadNetSamples() ([]NetSample, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var samples []NetSample
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // skip header lines
		}
		line := scanner.Text()
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:colonIdx])
		if iface == "lo" || strings.HasPrefix(iface, "docker") ||
			strings.HasPrefix(iface, "veth") || strings.HasPrefix(iface, "br-") {
			continue
		}
		fields := strings.Fields(line[colonIdx+1:])
		if len(fields) < 9 {
			continue
		}
		rxBytes, _ := strconv.ParseUint(fields[0], 10, 64)
		txBytes, _ := strconv.ParseUint(fields[8], 10, 64)
		samples = append(samples, NetSample{
			Iface:   iface,
			RxBytes: rxBytes,
			TxBytes: txBytes,
		})
	}
	return samples, scanner.Err()
}

func NetMetrics(prev, curr []NetSample, elapsed float64) []NetMetric {
	prevMap := make(map[string]NetSample)
	for _, s := range prev {
		prevMap[s.Iface] = s
	}

	var metrics []NetMetric
	for _, c := range curr {
		p, ok := prevMap[c.Iface]
		if !ok {
			continue
		}
		rxMBs := float64(c.RxBytes-p.RxBytes) / 1024 / 1024 / elapsed
		txMBs := float64(c.TxBytes-p.TxBytes) / 1024 / 1024 / elapsed
		if rxMBs < 0 {
			rxMBs = 0
		}
		if txMBs < 0 {
			txMBs = 0
		}
		metrics = append(metrics, NetMetric{
			Iface: c.Iface,
			RxMBs: rxMBs,
			TxMBs: txMBs,
		})
	}
	return metrics
}
