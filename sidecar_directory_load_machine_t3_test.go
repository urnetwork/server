//go:build (sidecar_localstack || sidecar_filecluster_t3) && sidecar_testhooks

// The machine's load average, which FS-86's card commit runs log beside their numbers (QD-1068; owner decision 598): a
// run on the local stack measures the machine under whatever else it runs (sidecar_directory_load_t3_test.go), and the
// layouts' hour is the row's reading only with nothing else running on it (sidecar_directory_load_layout_t3_test.go).

package server_test

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// The 1, 5 and 15 minute load averages, as "1.00 2.00 3.00", from /proc/loadavg on Linux and vm.loadavg on macOS, or
// why they did not read. A run logs this, so a reading it cannot take fails nothing.
func sidecarDirectoryLoadMachineAverage() string {
	var fields []string
	switch runtime.GOOS {
	case "linux":
		// "0.50 0.40 0.30 1/123 4567"
		content, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return fmt.Sprintf("unreadable (%v)", err)
		}
		fields = strings.Fields(string(content))
	case "darwin":
		// "{ 0.50 0.40 0.30 }"
		output, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
		if err != nil {
			return fmt.Sprintf("unreadable (sysctl vm.loadavg: %v)", err)
		}
		fields = strings.Fields(strings.NewReplacer("{", " ", "}", " ").Replace(string(output)))
	default:
		return fmt.Sprintf("unreadable on %s", runtime.GOOS)
	}
	if len(fields) < 3 {
		return fmt.Sprintf("unreadable (%q)", strings.Join(fields, " "))
	}
	averages := []string{}
	for _, field := range fields[:3] {
		average, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return fmt.Sprintf("unreadable (%q)", strings.Join(fields, " "))
		}
		averages = append(averages, strconv.FormatFloat(average, 'f', 2, 64))
	}
	return strings.Join(averages, " ")
}
