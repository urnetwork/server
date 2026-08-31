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
		"--write-out", httpsWriteOut,
		"https://" + hostname + path,
	}
	output, err := runner.local(ctx, "curl", args...)
	return exactHTTPSResult{
		values: parseKeyValueLines(output),
		output: output,
		err:    err,
	}
}

// runPublicHTTPS observes the user-facing DNS/CDN path. It deliberately does
// not follow redirects: a redirect or CDN-generated error is not a successful
// response for callers embedding the requested object.
func runPublicHTTPS(
	ctx context.Context,
	runner probeRunner,
	hostname string,
	path string,
) exactHTTPSResult {
	hostname = strings.TrimSpace(hostname)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	args := []string{
		"--http1.1", "--silent", "--show-error",
		"--connect-timeout", "3", "--max-time", "5", "--noproxy", "*",
		"--output", "/dev/null",
		"--write-out", httpsWriteOut,
		"https://" + hostname + path,
	}
	output, err := runner.local(ctx, "curl", args...)
	return exactHTTPSResult{
		values: parseKeyValueLines(output),
		output: output,
		err:    err,
	}
}

const httpsWriteOut = "\nmonitor_http_code=%{http_code}\n" +
	"monitor_exitcode=%{exitcode}\n" +
	"monitor_remote_ip=%{remote_ip}\n" +
	"monitor_content_type=%{content_type}\n" +
	"monitor_size_download=%{size_download}\n" +
	"monitor_time_total=%{time_total}\n"

func exactHTTPSHealthy(result exactHTTPSResult) bool {
	return result.err == nil &&
		result.values["monitor_exitcode"] == "0" &&
		result.values["monitor_http_code"] == "200"
}
