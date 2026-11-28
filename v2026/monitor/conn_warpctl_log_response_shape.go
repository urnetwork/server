// Classify only source-qualified terminal query responses already owned by the
// local runner; no response payload is retained or copied into findings.
package monitor

import (
	"errors"
	"os/exec"
	"regexp"
	"strings"
)

const warpctlLogNginx429Body = "<html>\n<head><title>429 Too Many Requests</title></head>\n<body>\n<center><h1>429 Too Many Requests</h1></center>\n<hr><center>nginx</center>\n</body>\n</html>"

// Warpctl 89a47cb logs() panics on Search failure and main() calls logs().
// Require its complete traceback as well as the runner's command boundary;
// unknown producer shapes and body-injected delimiters cannot supply a hint.
var warpctlLogOwnedStack = regexp.MustCompile(`^goroutine [1-9][0-9]* \[running\]:\nmain\.logs\([^()\r\n]*\)\n\t[^\r\n]+/warpctl/main\.go:[1-9][0-9]* \+0x[0-9a-f]+\nmain\.main\(\)\n\t[^\r\n]+/warpctl/main\.go:[1-9][0-9]* \+0x[0-9a-f]+\n$`)

// Only a fixed response shape leaves this function. It is not an attribution
// of the physical responder, loaded gateway policy or ingestion failure.
// Unknown producer versions/framing remain unknown, never a healthy result.
func warpctlLog429ResponseShape(err error, stderr string) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 ||
		len(stderr) > warpctlLogDiagnosticLimit ||
		warpctlLogDiagnosticClass(stderr) != observationErrorClassLogHttp429 {
		return "unknown"
	}
	// Nginx uses CRLF; Warpctl trims only the outside of the response body.
	text := strings.ReplaceAll(stderr, "\r\n", "\n")
	const boundary = "\n\ngoroutine "
	if strings.Count(text, boundary) != 1 {
		return "unknown"
	}
	before, stack, _ := strings.Cut(text, boundary)
	if !warpctlLogOwnedStack.MatchString("goroutine " + stack) {
		return "unknown"
	}
	start := 0
	if !strings.HasPrefix(before, "panic: ") {
		start = strings.LastIndex(before, "\npanic: ")
		if start < 0 {
			return "unknown"
		}
		start++
	}
	body, ok := strings.CutPrefix(before[start:], "panic: Loki query error (429): ")
	if !ok {
		return "unknown"
	}
	switch body {
	case "too many outstanding requests":
		return "loki-outstanding-requests"
	case warpctlLogNginx429Body:
		return "nginx-default-429"
	default:
		return "unknown"
	}
}
