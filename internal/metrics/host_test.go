package metrics

import (
	"runtime"
	"testing"
)

func TestReadHost(t *testing.T) {
	dir := t.TempDir()
	h := ReadHost(map[string]string{"A": dir, "B": dir, "Missing": dir + "/nope"})
	if h.CPUs < 1 {
		t.Fatalf("CPUs = %d", h.CPUs)
	}
	if runtime.GOOS != "linux" {
		return
	}
	if h.CPUTotal <= 0 || h.CPUBusy > h.CPUTotal {
		t.Errorf("cpu busy %v of %v", h.CPUBusy, h.CPUTotal)
	}
	if h.MemTotal <= 0 || h.MemUsed > h.MemTotal || h.ProcessRSS <= 0 {
		t.Errorf("memory used %v of %v, rss %v", h.MemUsed, h.MemTotal, h.ProcessRSS)
	}
	if len(h.Disks) != 1 || h.Disks[0].Name != "A, B" || h.Disks[0].Free > h.Disks[0].Total {
		t.Errorf("disks = %+v", h.Disks)
	}
}
