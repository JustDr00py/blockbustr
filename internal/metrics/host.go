package metrics

import (
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// Host is the machine blockbustr runs on, for the admin UI's Stats page.
// CPU figures are cumulative, like the counters beside them: the page takes
// two samples and works out the percentage from the difference. Everything
// comes from /proc and cgroups, so off Linux the fields stay zero. Memory
// and CPU count follow the container's cgroup limits when there are any.
type Host struct {
	CPUs       int
	CPUBusy    float64 // host CPU seconds spent working, all cores
	CPUTotal   float64 // host CPU seconds elapsed, all cores
	ProcessCPU float64 // CPU seconds used by blockbustr itself
	Load1      float64
	Load5      float64
	Load15     float64
	MemTotal   float64 // bytes; the container's limit if it has one
	MemUsed    float64 // bytes in use (the page cache doesn't count)
	ProcessRSS float64 // bytes resident in blockbustr
	Disks      []Disk
}

// Disk is the free space of the filesystem holding a directory.
type Disk struct {
	Name  string
	Total float64
	Free  float64
}

// ReadHost samples the host. Each of dirs ("name=path") that exists adds a
// Disk, one per filesystem.
func ReadHost(dirs map[string]string) Host {
	h := Host{CPUs: runtime.NumCPU()}
	if runtime.GOOS != "linux" {
		return h
	}
	// A clock tick is 100 Hz on every Linux build Go supports.
	const tick = 100.0
	if f := procFields("/proc/stat", "cpu"); len(f) >= 4 {
		var total, idle float64
		for i, v := range f {
			if i >= 8 { // guest time is already inside user time
				break
			}
			n, _ := strconv.ParseFloat(v, 64)
			total += n
			if i == 3 || i == 4 { // idle, iowait
				idle += n
			}
		}
		h.CPUTotal, h.CPUBusy = total/tick, (total-idle)/tick
	}
	if b, err := os.ReadFile("/proc/self/stat"); err == nil {
		// Fields after the ")" that ends the command name; utime and stime
		// are the 14th and 15th overall, so the 12th and 13th from there.
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 {
			if f := strings.Fields(s[i+1:]); len(f) > 13 {
				u, _ := strconv.ParseFloat(f[11], 64)
				k, _ := strconv.ParseFloat(f[12], 64)
				h.ProcessCPU = (u + k) / tick
			}
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 3 {
			h.Load1, _ = strconv.ParseFloat(f[0], 64)
			h.Load5, _ = strconv.ParseFloat(f[1], 64)
			h.Load15, _ = strconv.ParseFloat(f[2], 64)
		}
	}
	mem := meminfo()
	h.MemTotal = mem["MemTotal"]
	h.MemUsed = max(mem["MemTotal"]-mem["MemAvailable"], 0)
	h.ProcessRSS = mem["VmRSS"]
	if limit, ok := cgroupNumber("/sys/fs/cgroup/memory.max"); ok && limit > 0 && (h.MemTotal == 0 || limit < h.MemTotal) {
		h.MemTotal = limit
		if cur, ok := cgroupNumber("/sys/fs/cgroup/memory.current"); ok {
			// memory.current includes reclaimable page cache.
			h.MemUsed = max(cur-cgroupStat("/sys/fs/cgroup/memory.stat", "inactive_file"), 0)
		}
	}
	if quota := cpuQuota(); quota > 0 && quota < float64(h.CPUs) {
		h.CPUs = max(int(quota+0.5), 1)
	}
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		total, free, ok := diskUsage(dirs[name])
		if !ok {
			continue
		}
		// Directories on one filesystem report the same numbers: list it once.
		if i := slices.IndexFunc(h.Disks, func(d Disk) bool { return d.Total == total && d.Free == free }); i >= 0 {
			h.Disks[i].Name += ", " + name
			continue
		}
		h.Disks = append(h.Disks, Disk{Name: name, Total: total, Free: free})
	}
	return h
}

// procFields returns the fields after key on its line of file.
func procFields(file, key string) []string {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[0] == key {
			return f[1:]
		}
	}
	return nil
}

// meminfo is the host's /proc/meminfo and this process's VmRSS, in bytes.
func meminfo() map[string]float64 {
	out := map[string]float64{}
	read := func(file string) {
		b, err := os.ReadFile(file)
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			f := strings.Fields(v)
			if len(f) == 0 {
				continue
			}
			n, _ := strconv.ParseFloat(f[0], 64)
			if len(f) > 1 && f[1] == "kB" {
				n *= 1024
			}
			out[k] = n
		}
	}
	read("/proc/meminfo")
	read("/proc/self/status")
	return out
}

// cgroupNumber reads a single-number cgroup v2 file. "max" (no limit) and
// missing files report !ok.
func cgroupNumber(file string) (float64, bool) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return n, err == nil
}

func cgroupStat(file, key string) float64 {
	if f := procFields(file, key); len(f) > 0 {
		n, _ := strconv.ParseFloat(f[0], 64)
		return n
	}
	return 0
}

// cpuQuota is the CPUs a container may use (cgroup v2 cpu.max), or 0.
func cpuQuota() float64 {
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) != 2 || f[0] == "max" {
		return 0
	}
	q, _ := strconv.ParseFloat(f[0], 64)
	p, _ := strconv.ParseFloat(f[1], 64)
	if p <= 0 {
		return 0
	}
	return q / p
}
