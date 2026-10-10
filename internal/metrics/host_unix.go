//go:build unix

package metrics

import "syscall"

func diskUsage(dir string) (total, free float64, ok bool) {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0, 0, false
	}
	return float64(st.Blocks) * float64(st.Bsize), float64(st.Bavail) * float64(st.Bsize), true
}
