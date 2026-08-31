package monitor

import (
	"context"
	"fmt"
	"strings"
)

// exactHTTPSResult is the shared public-ingress observation used by focused
// signals. Hostname remains the TLS SNI/HTTP host while Address pins one
// configured edge, so DNS health selection cannot substitute a sibling.
type exactHTTPSResult struct {
	values map[string]string
	output string
	err    error
}

func runExactHTTPS(
	ctx context.Context,
	runner probeRunner,
	hostname string,
	address string,
	path string,
) exactHTTPSResult {
	hostname = strings.TrimSpace(hostname)
	address = strings.TrimSpace(address)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	resolve := fmt.Sprintf("%s:443:[%s]", hostname, address)
	args := []string{
		"--ipv6", "--http1.1", "--silent", "--show-error",
		"--connect-timeout", "3", "--max-time", "5", "--noproxy", "*",
		"--resolve", resolve,
		"--output", "/dev/null",
		"--write-out", "\nmonitor_http_code=%{http_code}\nmonitor_exitcode=%{exitcode}\nmonitor_remote_ip=%{remote_ip}\nmonitor_time_total=%{time_total}\n",
		"https://" + hostname + path,
	}
	output, err := runner.local(ctx, "curl", args...)
	return exactHTTPSResult{
		values: parseKeyValueLines(output),
		output: output,
		err:    err,
	}
}

func exactHTTPSHealthy(result exactHTTPSResult) bool {
	return result.err == nil &&
		result.values["monitor_exitcode"] == "0" &&
		result.values["monitor_http_code"] == "200"
}
