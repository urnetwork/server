// Synthetic streams and a fixture-only ssh executable exercise the actual
// intake seam. No host, credential or production endpoint is used.
package monitor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A selected subprocess emits deterministic streams without a network tool.
func TestMimirShellHelperProcess(t *testing.T) {
	args := os.Args
	mode := ""
	for index, arg := range args {
		if arg == "--mimir-intake-helper" && index+1 < len(args) {
			mode = args[index+1]
		}
	}
	switch mode {
	case "healthy":
		_, _ = io.WriteString(os.Stdout, `{"status":"success"}`)
	case "stdout":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", mimirShellResponseMaxBytes+1))
	case "stdout-small":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 65537))
	case "stderr":
		_, _ = io.WriteString(os.Stderr, strings.Repeat("x", mimirShellStderrMaxBytes+1))
	case "private-error":
		_, _ = io.WriteString(os.Stderr, "private-intake-canary")
		os.Exit(7)
	default:
		return
	}
	os.Exit(0)
}

// The old generic bytes.Buffer seam succeeds on oversized streams; this
// regression requires the typed failure and zero exposed bytes instead.
func TestMimirShellNativeIntakeBoundaries(t *testing.T) {
	for _, mode := range []string{"healthy", "stdout", "stdout-small", "stderr", "private-error"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			script := "#!/bin/sh\nexec " + shellSingleQuote(os.Args[0]) + " -test.run=^TestMimirShellHelperProcess$ -- --mimir-intake-helper " + mode + "\n"
			if err := os.WriteFile(filepath.Join(directory, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", directory)
			limit := mimirShellResponseMaxBytes
			if mode == "stdout-small" {
				limit = 65536
			}
			ctx := context.WithValue(context.Background(), mimirShellContextKey{}, limit)
			output, diagnostics, err := runSSHCommand(ctx, []string{"synthetic-target", "true"}, "")
			if mode == "healthy" {
				if err != nil || output != `{"status":"success"}` || len(output) > limit || len(diagnostics) > mimirShellStderrMaxBytes {
					t.Fatal("within-bound native observation changed")
				}
				return
			}
			if err == nil || output != "" || diagnostics != "" {
				t.Fatal("failed native intake exposed bytes or accepted overflow")
			}
			if strings.Contains(err.Error(), "private-intake-canary") {
				t.Fatal("private diagnostic escaped intake")
			}
			var boundary *observationBoundaryError
			if !errors.As(err, &boundary) || boundary.boundary != observationBoundaryNativeWait {
				t.Fatal("native wait provenance lost")
			}
			if mode != "private-error" {
				var overflow *mimirShellOutputError
				expectedStream := mode
				if mode == "stdout-small" {
					expectedStream = "stdout"
				}
				if !errors.As(err, &overflow) || overflow.stream != expectedStream {
					t.Fatal("native overflow lost fixed typed stream")
				}
			}
		})
	}
}

// The exact limit is accepted; an extra byte is sticky and never grows memory.
func TestMimirShellWriterBoundary(t *testing.T) {
	canceled := 0
	writer := &mimirShellWriter{limit: 3, cancel: func() { canceled++ }}
	if count, err := writer.Write([]byte("abc")); count != 3 || err != nil || canceled != 0 {
		t.Fatal("exact bound rejected")
	}
	if count, err := writer.Write([]byte("private-intake-canary")); count != 0 || err == nil || canceled != 1 {
		t.Fatal("extra byte did not stop intake")
	}
	if writer.buffer.String() != "abc" || !writer.exceeded {
		t.Fatal("overflow grew retained prefix")
	}
	if count, err := writer.Write([]byte("more")); count != 0 || err == nil || canceled != 1 {
		t.Fatal("overflow was not sticky")
	}
}

// Every affected public signal must publish unknown before parsing a prefix,
// and must not replay the same oversized query through another gateway.
func TestMimirShellSignalsOverflowCannotObserve(t *testing.T) {
	signals := []Signal{NewMissingOriginSignal(), NewProvenanceSignal(), NewProviderCountSignal(), NewProxyCacheSignal(), NewProxyPoolSignal(), NewProxyRuntimeSignal()}
	for _, signal := range signals {
		t.Run(signal.Key(), func(t *testing.T) {
			calls := 0
			source := &mimirShellPolicySource{syntheticSource: &syntheticSource{}, read: func(ctx context.Context, command, _ string, _ time.Duration) (string, error) {
				calls++
				if !strings.Contains(command, "--max-time 15 ") || strings.Contains(command, "--max-filesize") || ctx.Value(mimirShellContextKey{}) != mimirShellResponseMaxBytes {
					t.Fatal("original injected command, scoped policy or timeout changed")
				}
				return strings.Repeat(" ", mimirShellResponseMaxBytes+1), nil
			}}
			settings := syntheticSettings(source)
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-a", Roles: []string{"services"}}, HostSettings{Name: "metrics-b", Roles: []string{"services"}})
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || calls != 1 || len(alerts) != 1 {
				t.Fatal("overflow retried or lost explicit visibility")
			}
			alert := requireAlertClass(t, alerts, "cannot-observe")
			if !strings.Contains(alert.Observed, "source_phase=output-bound stream=stdout") {
				t.Fatal("overflow lost finite source boundary")
			}
			requireAlertOmits(t, alert, "private-intake-canary")
		})
	}
}

// Parent cancellation cannot start a native executable or accept a synthetic
// response that completed after its owner ended.
func TestMimirShellCancellationRejectsCompletedPrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	output, diagnostics, err := runMimirShellCommand(ctx, "not-an-executable", nil, "", mimirShellResponseMaxBytes, "")
	if !errors.Is(err, context.Canceled) || output != "" || diagnostics != "" {
		t.Fatal("pre-canceled intake reached execution")
	}
	ctx, cancel = context.WithCancel(context.Background())
	calls := 0
	runner := &sourceRunner{source: &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) { calls++; cancel(); return `{"status":"success"}`, nil }}}
	output, selected, err := mimirShellFirstServiceGateway(ctx, runner, []*host{{name: "synthetic-a"}, {name: "synthetic-b"}}, nil, "synthetic-command", mimirShellResponseMaxBytes)
	if !errors.Is(err, context.Canceled) || output != "" || selected != nil || calls != 1 {
		t.Fatal("completed canceled source became usable or retried")
	}
}

// Context and timeout capture exercise the same source seams as real probes.
type mimirShellPolicySource struct {
	*syntheticSource
	read func(context.Context, string, string, time.Duration) (string, error)
}

// Shell observations retain the per-caller byte policy in their context.
func (self *mimirShellPolicySource) Host(ctx context.Context, _ HostSettings, command string) (string, error) {
	return self.read(ctx, command, "", 0)
}

// The stdin query keeps the exact original body and timeout.
func (self *mimirShellPolicySource) HostStdin(ctx context.Context, _ HostSettings, command, stdin string, timeout time.Duration) (string, error) {
	return self.read(ctx, command, stdin, timeout)
}

// Small explicit policies cannot be silently raised to the new default.
func TestMimirShellPreservesCallerPolicy(t *testing.T) {
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) { calls++; return "", nil }}
	for _, invalid := range []int{0, -1} {
		output, err := mimirShell(context.Background(), &sourceRunner{source: source}, &host{name: "synthetic"}, "query", invalid)
		var overflow *mimirShellOutputError
		if output != "" || !errors.As(err, &overflow) || overflow.stream != "policy" || calls != 0 {
			t.Fatal("invalid policy reached source or became healthy")
		}
	}
	for _, limit := range []int{65536, 1024 * 1024, 2 * 1024 * 1024, 4 * 1024 * 1024} {
		for _, excess := range []int{0, 1} {
			source := &mimirShellPolicySource{syntheticSource: &syntheticSource{}, read: func(ctx context.Context, _ string, _ string, _ time.Duration) (string, error) {
				if ctx.Value(mimirShellContextKey{}) != limit {
					t.Fatal("caller policy lost before source")
				}
				return strings.Repeat("x", limit+excess), nil
			}}
			runner := &sourceRunner{source: source}
			output, err := mimirShell(context.Background(), runner, &host{name: "synthetic"}, "query", limit)
			if excess == 0 {
				if err != nil || len(output) != limit {
					t.Fatal("exact caller bound rejected")
				}
			} else {
				var overflow *mimirShellOutputError
				if output != "" || !errors.As(err, &overflow) {
					t.Fatal("caller policy raised or prefix exposed")
				}
			}
		}
	}
}

// The API stdin route must preserve its existing20s deadline and private body.
func TestMimirShellPreservesStdinTimeout(t *testing.T) {
	calls := 0
	source := &mimirShellPolicySource{syntheticSource: &syntheticSource{}, read: func(ctx context.Context, command, stdin string, timeout time.Duration) (string, error) {
		calls++
		if command != "synthetic-command" || stdin != "private-stdin-canary" || timeout != 20*time.Second || ctx.Value(mimirShellContextKey{}) != 65536 {
			t.Fatal("stdin route changed request contract")
		}
		return "healthy", nil
	}}
	output, err := mimirShellStdin(context.Background(), &sourceRunner{source: source}, &host{name: "synthetic"}, "synthetic-command", "private-stdin-canary", 20*time.Second, 65536)
	if err != nil || output != "healthy" || calls != 1 {
		t.Fatal("stdin query was replayed or lost")
	}
}

// SSH's fixed timeout marker keeps its authority without returning stderr.
func TestMimirShellPrivateNativeDiagnostic(t *testing.T) {
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		return "", "", &mimirShellNativeError{err: syntheticProcessExit(t, 255), stderr: "ssh: connect to host 192.0.2.91 port 22: Connection timed out"}
	}
	_, err := mimirShell(context.Background(), runner, &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command", 65536)
	var native *sshCommandError
	if !errors.As(err, &native) || !native.connectTimeoutDiagnostic || strings.Contains(err.Error(), "192.0.2.91") || strings.Contains(err.Error(), "Connection timed out") {
		t.Fatal("fixed native timeout authority or diagnostic privacy lost")
	}
}

// Ordinary transport failures retain the original ordered gateway fallback.
func TestMimirShellGatewayFallbackPolicy(t *testing.T) {
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("synthetic-source-unavailable")
		}
		return "healthy", nil
	}}
	hosts := []*host{{name: "synthetic-a"}, {name: "synthetic-b"}}
	output, selected, err := mimirShellFirstServiceGateway(context.Background(), &sourceRunner{source: source}, hosts, nil, "query", 65536)
	if err != nil || output != "healthy" || selected != hosts[1] || calls != 2 {
		t.Fatal("ordinary gateway fallback changed")
	}
}

// The new context policy cannot bypass an excluded owner of the dial address.
func TestMimirShellKeepsHostScopeGuards(t *testing.T) {
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) { calls++; return "healthy", nil }}
	settings := syntheticSettings(source)
	settings.AddressMode = AddressModeLAN
	settings.Hosts = []HostSettings{
		{Name: "excluded.example.test", LANAddress: "192.0.2.1"},
		{Name: "allowed.example.test", LANAddress: "192.0.2.1", Roles: []string{"services", "pg-primary", "redis-cluster"}},
	}
	settings, err := ExcludeHosts(settings, "excluded.example.test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	output, err := mimirShell(context.Background(), env.runner, env.cfg.hosts[1], "query", 65536)
	if err == nil || output != "" || calls != 0 {
		t.Fatal("bounded direct query bypassed excluded endpoint ownership")
	}
	output, selected, err := mimirShellFirstServiceGateway(context.Background(), env.runner, []*host{env.cfg.hosts[1]}, nil, "query", 65536)
	if err == nil || output != "" || selected != nil || calls != 0 {
		t.Fatal("bounded gateway query bypassed excluded endpoint ownership")
	}
}

// Successful bounded native diagnostics stay private even though the legacy
// primitive's success API permits returned stderr. This is a new-hook policy,
// separate from the common within-bound healthy contract above.
func TestMimirShellScopedSuccessfulDiagnosticsArePrivate(t *testing.T) {
	directory := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '{\"status\":\"success\"}'\nprintf '%s' 'private-success-diagnostic-canary' >&2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(directory, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
	ctx := context.WithValue(context.Background(), mimirShellContextKey{}, mimirShellResponseMaxBytes)
	output, err := runner.shell(ctx, &host{name: "synthetic", overlayIp: "192.0.2.1"}, "synthetic-command")
	if err != nil || output != `{"status":"success"}` {
		t.Fatal("successful native diagnostics changed the query result")
	}
	// Assert at the native adapter too: the public runner cannot retrieve
	// discarded bytes, rather than relying only on its stdout-only return.
	output, diagnostics, err := runSSHCommand(ctx, []string{"synthetic-target", "true"}, "")
	if err != nil || output != `{"status":"success"}` || diagnostics != "" {
		t.Fatal("scoped successful diagnostics escaped the private adapter")
	}
}

// A retired owner retains independently observed source identity even when
// that source also returns oversized partial bytes; neither can become data.
func TestMimirShellEndedParentKeepsObservedSource(t *testing.T) {
	for _, seam := range []string{"gateway", "gateway-parser", "direct", "direct-parser", "stdin", "stdin-parser"} {
		t.Run(seam, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sourceCause := errors.New("private-observed-source-canary")
			calls := 0
			source := &mimirShellPolicySource{syntheticSource: &syntheticSource{}, read: func(context.Context, string, string, time.Duration) (string, error) {
				calls++
				cancel()
				return strings.Repeat("x", 128), sourceCause
			}}
			runner := &sourceRunner{source: source}
			hosts := []*host{{name: "synthetic-a"}, {name: "synthetic-b"}}
			var output string
			var err error
			switch seam {
			case "gateway":
				output, _, err = mimirShellFirstServiceGateway(ctx, runner, hosts, nil, "query", 64)
			case "gateway-parser":
				output, _, err = mimirShellFirstServiceGatewayParsed(ctx, runner, hosts, nil, "query", 64)
			case "direct":
				output, err = mimirShell(ctx, runner, hosts[0], "query", 64)
			case "direct-parser":
				output, err = mimirShellParsed(ctx, runner, hosts[0], "query", 64)
			case "stdin":
				output, err = mimirShellStdin(ctx, runner, hosts[0], "query", "body", time.Second, 64)
			case "stdin-parser":
				output, err = mimirShellStdinParsed(ctx, runner, hosts[0], "query", "body", time.Second, 64)
			}
			if output != "" || calls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, sourceCause) || strings.Contains(err.Error(), sourceCause.Error()) {
				t.Fatal("retired source lost cause, leaked bytes or started another attempt")
			}
		})
	}
}

// Retirement can retain a pure safe rejection cause, but never a successful
// sample, an oversized body, a replaced transport cause or another read.
func TestMimirShellRetiredRejectionRetainsOnlySafeFailure(t *testing.T) {
	for _, mode := range []string{"rejected", "successful", "oversized", "transport", "native-bound"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			safeRejection := errors.New("fixed-safe-rejection")
			transportCause := errors.New("private-transport-cause")
			calls, classifications := 0, 0
			source := &syntheticSource{hostFn: func(_ HostSettings, _ string) (string, error) {
				calls++
				cancel()
				switch mode {
				case "oversized":
					return strings.Repeat("x", 65), nil
				case "transport":
					return "rejected", transportCause
				case "native-bound":
					return "", &mimirShellOutputError{stream: "stdout"}
				}
				return mode, nil
			}}
			classifier := func(body string) error {
				classifications++
				if body == "rejected" {
					return safeRejection
				}
				return nil
			}
			hosts := []*host{{name: "synthetic-a"}, {name: "synthetic-b"}}
			output, selected, err := mimirShellFirstServiceGatewayWithRejection(ctx, &sourceRunner{source: source}, hosts, nil, "query", 64, classifier)
			if output != "" || selected != nil || calls != 1 || !errors.Is(err, context.Canceled) || err.Error() != context.Canceled.Error() {
				t.Fatal("retired classifier published data, lost owner or replayed")
			}
			expectedClassifications := 0
			if mode == "rejected" || mode == "successful" {
				expectedClassifications = 1
			}
			if classifications != expectedClassifications || errors.Is(err, safeRejection) != (mode == "rejected") || errors.Is(err, transportCause) != (mode == "transport") {
				t.Fatal("retired classifier replaced or fabricated a source cause")
			}
			if mode == "native-bound" {
				var overflow *mimirShellOutputError
				if !errors.As(err, &overflow) || overflow.stream != "stdout" {
					t.Fatal("retirement lost native refusal identity")
				}
			}
		})
	}
}
