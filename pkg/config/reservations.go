package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func defaultKubeReserved(cpuCount, totalMemoryMi, maxPods int) map[string]string {
	// AKS uses progressive CPU brackets and caps its max-pod-based memory
	// reservation at 25% of host memory.
	// https://learn.microsoft.com/azure/aks/node-resource-reservations
	reservedCPUMilli := 0
	for _, bracket := range []struct {
		cores int
		rate  int
	}{
		{cores: 1, rate: 60},
		{cores: 1, rate: 40},
		{cores: 2, rate: 20},
		{cores: cpuCount, rate: 10},
	} {
		cores := min(cpuCount, bracket.cores)
		reservedCPUMilli += cores * bracket.rate
		cpuCount -= cores
		if cpuCount == 0 {
			break
		}
	}

	reservedMemoryMi := int64(max(maxPods, 0))*20 + 50
	if memoryLimitMi := int64(totalMemoryMi) / 4; memoryLimitMi > 0 {
		reservedMemoryMi = min(reservedMemoryMi, memoryLimitMi)
	}

	return map[string]string{
		"cpu":    fmt.Sprintf("%dm", reservedCPUMilli),
		"memory": fmt.Sprintf("%dMi", reservedMemoryMi),
	}
}

func hostTotalMemoryMi() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}

	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		totalKi, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return int(totalKi / 1024)
	}

	return 0
}
