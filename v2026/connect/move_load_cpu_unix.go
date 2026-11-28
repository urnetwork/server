//go:build unix

package connect

import (
	"syscall"
	"time"
)

// The process's user and system CPU time, from its rusage.
func moveProcessCpuTime() (time.Duration, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano()), true
}
