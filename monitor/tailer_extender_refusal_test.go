// All records in these controls are synthetic Warp log entries.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestConnectionRefusalTaskworkerExemplar(t *testing.T) {
	const wrapper = "[synthetic-edge-with-a-long-name][taskworker][g1][cid:012345abcdef][I][2026-11-20T04:38:44.000001Z][extender_probe_diagnostics.go:509]"
	const message = `[extenderprobe]diagnostic extender=11111111-1111-1111-1111-111111111111 ipv=4 class=dial_refused detail="dial: connect: connection refused" server_name=private.example elapsed=1.5s connect=none server_bytes=0 since_activation=7m0s since_uptime_ok=never failures_before=2`
	line := wrapper + message
	if strings.Contains(truncateLine(line), "class=") || !strings.Contains(truncateLine(line), "[extenderprobe]diagnostic") {
		t.Fatal("fixture must lose the class in the original 200-byte prefix")
	}
	for _, test := range []struct {
		name, service, first, sample string
	}{
		{"ipv4", "taskworker", line, "[extenderprobe]diagnostic exemplar ipv=4 class=dial_refused"},
		{"ipv6 flattened error", "taskworker", strings.NewReplacer("ipv=4", "ipv=6", "class=dial_refused", "class=dial_error", "dial: connect", "quoted connect").Replace(line), "[extenderprobe]diagnostic exemplar ipv=6 class=dial_error"},
		{"wrong selected stream", "proxy", line, ""},
		{"wrong outer service", "taskworker", strings.Replace(line, "[taskworker]", "[proxy]", 1), ""},
		{"wrong severity", "taskworker", strings.Replace(line, "[I]", "[E]", 1), ""},
		{"missing outer identity", "taskworker", message, ""},
		{"wrong source", "taskworker", strings.Replace(line, "extender_probe_diagnostics.go:509", "another.go:509", 1), ""},
		{"unverified line number", "taskworker", strings.Replace(line, ".go:509", ".go:510", 1), ""},
		{"quoted logger", "taskworker", "[synthetic-edge][taskworker][g1][cid:012345abcdef][I][2026-11-20T04:38:44Z][another.go:11] quoted " + line, ""},
		{"marker inside text", "taskworker", wrapper + "quoted " + message, ""},
		{"nested marker in detail", "taskworker", strings.Replace(line, "dial: connect:", "[extenderprobe]diagnostic quoted connect:", 1), ""},
		{"extra fields", "taskworker", line + " result=other", ""},
		{"missing ending", "taskworker", strings.TrimSuffix(line, " failures_before=2"), ""},
		{"unknown class", "taskworker", strings.Replace(line, "class=dial_refused", "class=unreviewed", 1), ""},
		{"inconsistent HTTP detail", "taskworker", strings.Replace(line, "class=dial_refused", "class=refused", 1), ""},
		{"invalid server name", "taskworker", strings.Replace(line, "private.example", "..", 1), ""},
		{"unknown family", "taskworker", strings.Replace(line, "ipv=4", "ipv=9", 1), ""},
		{"bad elapsed time", "taskworker", strings.Replace(line, "elapsed=1.5s", "elapsed=nonsense", 1), ""},
		{"bad timestamp", "taskworker", strings.Replace(line, "11-20T", "19-49T", 1), ""},
		{"bad escaping", "taskworker", strings.Replace(line, "dial: connect:", `bad\Q connect:`, 1), ""},
		{"oversized detail", "taskworker", strings.Replace(line, "dial: connect:", strings.Repeat("x", 170)+" connect:", 1), ""},
		{"mixed records", "taskworker", line + " " + line, ""},
		{"unqualified refusal elsewhere", "taskworker", "connect: connection refused " + strings.Replace(line, "dial: connect: connection refused", "EOF", 1), ""},
		{"other logger first", "taskworker", "ordinary connect: connection refused", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			lines := []string{test.first}
			for i := 1; i < 19; i++ {
				lines = append(lines, fmt.Sprintf("other refusal %d connect: connection refused", i))
			}
			// A later qualified exemplar must never replace the first.
			lines = append(lines, line)
			source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
				if len(args) > 1 && args[0] == "ls" {
					return "repo names synthetic-" + test.service, nil
				}
				return strings.Join(lines, "\n"), nil
			}}
			alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			alert := requireAlertClass(t, alerts, "connection-refused")
			if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Frame != "" ||
				alert.Identity() != "logs/connection-refused|connection-refused|"+test.service+"|" ||
				alert.Observed != "rate=20/min class=connection-refused target="+test.service {
				t.Fatalf("unexpected refusal aggregation: %+v", alert)
			}
			_, sample, ok := strings.Cut(alert.Evidence, "\nsample: ")
			want := test.sample
			if want == "" {
				want = truncateLine(test.first)
			}
			if !ok || sample != want {
				t.Fatalf("exemplar sample %q, want %q", sample, want)
			}
		})
	}
}
