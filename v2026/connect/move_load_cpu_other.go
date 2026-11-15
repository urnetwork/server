//go:build !unix

package connect

import (
	"time"
)

// No rusage here: the process CPU is not measured.
func moveProcessCpuTime() (time.Duration, bool) {
	return 0, false
}
