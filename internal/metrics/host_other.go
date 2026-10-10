//go:build !unix

package metrics

func diskUsage(string) (total, free float64, ok bool) { return 0, 0, false }
