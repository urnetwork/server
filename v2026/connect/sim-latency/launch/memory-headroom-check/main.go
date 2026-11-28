// Checks actual host headroom before an operator-owned pressure test. This
// read-only observation never grants evaluation or host-admission authority.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// The caller owns its required byte reserve. Missing/nonpositive refuses.
type settings struct {
	minimumAvailableBytes int64
	maximumMeminfoBytes   int64
}

// Bounds the small kernel document without imposing another scoring policy.
func defaultSettings() settings {
	return settings{maximumMeminfoBytes: 64 * 1024}
}

// A duplicate, malformed or overflowing available-memory field is unknown,
// not zero or permission to start a memory bomb.
func availableMemoryBytes(content []byte) (int64, error) {
	var value int64
	found := false
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "MemAvailable:" {
			continue
		}
		if found || len(fields) != 3 || fields[2] != "kB" {
			return 0, errors.New("ambiguous host available-memory field")
		}
		amount, err := strconv.ParseUint(fields[1], 10, 63)
		if err != nil || strconv.FormatUint(amount, 10) != fields[1] || amount > math.MaxInt64/1024 {
			return 0, errors.New("invalid host available-memory value")
		}
		value, found = int64(amount)*1024, true
	}
	if !found {
		return 0, errors.New("host available-memory field missing")
	}
	return value, nil
}

// The inclusive comparison is only a prerequisite to running a pressure test.
func checkHeadroom(content []byte, config settings) (int64, bool, error) {
	if config.minimumAvailableBytes <= 0 || config.maximumMeminfoBytes <= 0 || int64(len(content)) > config.maximumMeminfoBytes {
		return 0, false, errors.New("headroom check has no finite positive owner")
	}
	available, err := availableMemoryBytes(content)
	return available, err == nil && available >= config.minimumAvailableBytes, err
}

// Emits to stderr so the tested program's stdout remains an exact JSON report.
func main() {
	config := defaultSettings()
	flag.Int64Var(&config.minimumAvailableBytes, "minimum-bytes", 0, "Required available host bytes; nonpositive refuses")
	flag.Parse()
	if flag.NArg() != 0 || config.minimumAvailableBytes <= 0 {
		fmt.Fprintln(os.Stderr, "a positive explicit host-memory reserve is required")
		os.Exit(2)
	}
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		fmt.Fprintln(os.Stderr, "host memory information unavailable")
		os.Exit(1)
	}
	content, readErr := io.ReadAll(io.LimitReader(file, config.maximumMeminfoBytes+1))
	closeErr := file.Close()
	available, passed, checkErr := checkHeadroom(content, config)
	if readErr != nil || closeErr != nil || checkErr != nil {
		fmt.Fprintln(os.Stderr, "host memory information failed bounded validation")
		os.Exit(1)
	}
	err = json.NewEncoder(os.Stderr).Encode(map[string]any{"schema": 1, "kind": "host-pressure-headroom-observation/v1", "available_bytes": available,
		"minimum_available_bytes": config.minimumAvailableBytes, "passed": passed, "grading_ready": false})
	if err != nil || !passed {
		os.Exit(1)
	}
}
