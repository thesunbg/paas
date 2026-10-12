package agent

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/thesunbg/paas/internal/proto"
)

func readMeminfo() map[string]int {
	out := map[string]int{}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 {
			n, _ := strconv.Atoi(fields[1])
			out[strings.TrimSuffix(fields[0], ":")] = n // kB
		}
	}
	return out
}

func memTotalMB() int { return readMeminfo()["MemTotal"] / 1024 }

func readCPU() (idle, total uint64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	for i, f := range fields[1:] {
		v, _ := strconv.ParseUint(f, 10, 64)
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return idle, total
}

func psiMemorySome10() float64 {
	data, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "some") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "avg10=") {
				v, _ := strconv.ParseFloat(strings.TrimPrefix(f, "avg10="), 64)
				return v
			}
		}
	}
	return 0
}

func (a *Agent) usage() proto.NodeUsage {
	mi := readMeminfo()
	u := proto.NodeUsage{
		MemoryAvailableMB: mi["MemAvailable"] / 1024,
		MemoryUsedMB:      (mi["MemTotal"] - mi["MemAvailable"]) / 1024,
		PSIMemorySome10:   psiMemorySome10(),
	}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(data)); len(f) > 0 {
			u.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	idle, total := readCPU()
	a.cpuMu.Lock()
	if a.lastCPU[1] > 0 && total > a.lastCPU[1] {
		dt := float64(total - a.lastCPU[1])
		di := float64(idle - a.lastCPU[0])
		u.CPUPercent = (1 - di/dt) * 100
	}
	a.lastCPU = [2]uint64{idle, total}
	a.cpuMu.Unlock()
	var st syscall.Statfs_t
	if err := syscall.Statfs(a.cfg.DataDir, &st); err == nil {
		total := float64(st.Blocks) * float64(st.Bsize)
		free := float64(st.Bavail) * float64(st.Bsize)
		u.DiskTotalGB = total / (1 << 30)
		u.DiskUsedGB = (total - free) / (1 << 30)
	}
	return u
}
