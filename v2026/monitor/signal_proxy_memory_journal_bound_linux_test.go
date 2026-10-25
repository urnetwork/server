package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These are diagnostic observation budgets proposed for this collector, not
// limits on the host journal, service logging, or production resource policy.
const proxyJournalTestBytes = 4 * 1024 * 1024

// The helper is selected only by the fake journalctl executable in this file.
// It never reads a real journal or invokes a network/host-observation command.
func TestProxyKernelJournalLocalProducerHelper(t *testing.T) {
	if os.Getenv("PROXY_JOURNAL_FAKE") != "1" {
		return
	}
	signal.Ignore(syscall.SIGPIPE)
	status := 0
	written := 0
	capacity, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(3), uintptr(1032), 0) // Linux F_GETPIPE_SZ
	if errno != 0 {
		os.Exit(91)
	}
	reverse := false
	for _, arg := range os.Args {
		if arg == "--reverse" || arg == "-r" {
			reverse = true
		}
	}
	dataOut := os.NewFile(uintptr(3), "synthetic-journal-stdout")
	dataErr := os.NewFile(uintptr(4), "synthetic-journal-stderr")
	mode := os.Getenv("PROXY_JOURNAL_MODE")
	write := func(s string) bool {
		stream := dataOut
		n, err := io.WriteString(stream, s)
		written += n
		return err == nil
	}
	switch mode {
	case "healthy":
		write("ordinary synthetic kernel record\n")
	case "at-cap", "over-cap", "cut-with-oom":
		n := proxyJournalTestBytes
		if mode != "at-cap" {
			n *= 2
		}
		if mode == "cut-with-oom" {
			write("Out of memory: Killed process 102 (bringyour-proxy) synthetic_new\n")
		}
		line := strings.Repeat("x", 255) + "\n"
		for remaining := n; remaining > 0; remaining -= len(line) {
			if !write(line) {
				break
			}
		}
	case "oversized-record":
		write(strings.Repeat("x", proxyJournalTestBytes) + "\n")
	case "stderr-forgery":
		n, _ := io.WriteString(dataErr, "Out of memory: Killed process 102 (bringyour-proxy) synthetic_stderr\n")
		written += n
	case "oom-with-stderr":
		write("Out of memory: Killed process 102 (bringyour-proxy) synthetic_new\n")
		n, _ := io.WriteString(dataErr, "synthetic journal diagnostic\n")
		written += n
	case "tail-with-oom":
		write("Out of memory: Killed process 102 (bringyour-proxy) synthetic_new\n")
		write("synthetic unterminated tail")
	case "blocked":
		ended := make(chan os.Signal, 1)
		signal.Notify(ended, syscall.SIGTERM, syscall.SIGINT)
		<-ended
		status = 143
	case "unterminated-record":
		write("synthetic record whose trailing newline is missing")
	case "oom", "source-failed":
		older := "Out of memory: Killed process 101 (bringyour-proxy) synthetic_old\n"
		newer := "Out of memory: Killed process 102 (bringyour-proxy) synthetic_new\n"
		if reverse {
			write(newer)
			write(older)
		} else {
			write(older)
			write(newer)
		}
		if mode == "source-failed" {
			status = 7
		}
	default:
		status = 92
	}
	receipt := struct {
		Written int  `json:"written"`
		Pipe    int  `json:"pipe"`
		Status  int  `json:"status"`
		Reverse bool `json:"reverse"`
		PID     int  `json:"pid"`
	}{written, int(capacity), status, reverse, os.Getpid()}
	raw, err := json.Marshal(receipt)
	if err != nil {
		os.Exit(93)
	}
	file, err := os.OpenFile(os.Getenv("PROXY_JOURNAL_RECEIPT"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(94) // Also rejects a second fake-journal invocation.
	}
	if _, err = file.Write(raw); err != nil {
		os.Exit(95)
	}
	if err = file.Close(); err != nil {
		os.Exit(96)
	}
	os.Exit(status)
}

type proxyJournalCaptureBuffer struct {
	buffer bytes.Buffer
}

func (self *proxyJournalCaptureBuffer) Len() int {
	return self.buffer.Len()
}

func (self *proxyJournalCaptureBuffer) String() string {
	return self.buffer.String()
}

func (self *proxyJournalCaptureBuffer) Write(p []byte) (int, error) {
	if len(p) > 64*1024-self.Len() {
		return 0, fmt.Errorf("synthetic collector output bound exceeded")
	}
	return self.buffer.Write(p)
}

type proxyJournalProducerReceipt struct {
	PID     int  `json:"pid"`
	Written int  `json:"written"`
	Pipe    int  `json:"pipe"`
	Status  int  `json:"status"`
	Reverse bool `json:"reverse"`
}

// Run the exact emitted journal suffix. The declared seam removes unrelated
// /proc, ps, systemctl and resource reads. Nothing replaces or edits the journal
// block itself, and the fake invocation receipt proves this is not no-proxy exit.
func proxyJournalActualCollector(t *testing.T, mode string) ([]Alert, map[string]string, proxyJournalProducerReceipt) {
	t.Helper()
	dir := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("could not locate the local synthetic test helper")
	}
	journal := "#!/bin/sh\nexec \"$PROXY_JOURNAL_BINARY\" -test.run=^TestProxyKernelJournalLocalProducerHelper$ -- \"$@\" 3>&1 4>&2 1>/dev/null 2>/dev/null\n"
	if err = os.WriteFile(filepath.Join(dir, "journalctl"), []byte(journal), 0700); err != nil {
		t.Fatal("could not create fake journal command")
	}
	for _, name := range []string{"home", "warp", "site", "vault", "config"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal("could not create private synthetic helper roots")
		}
	}
	receiptPath := filepath.Join(dir, "producer.json")
	fields := map[string]string{}
	calls := 0
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		calls++
		boundary := "}' /proc/net/snmp\n\n"
		if strings.Count(command, boundary) != 1 || !strings.Contains(command, proxyMemoryMarker) {
			t.Fatal("actual emitted journal boundary changed")
		}
		_, journalBlock, _ := strings.Cut(command, boundary)
		if !strings.Contains(journalBlock, "journalctl") {
			t.Fatal("actual journal block was not selected")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/bash", "-c", "set +e\nset +o pipefail\n"+journalBlock)
		cmd.WaitDelay = 3 * time.Second
		if mode == "blocked" {
			cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		}
		cmd.Env = []string{
			"PATH=" + dir + ":/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC0", "TMPDIR=" + dir,
			"GOMAXPROCS=2",
			"HOME=" + filepath.Join(dir, "home"), "WARP_ENV=synthetic",
			"WARP_HOME=" + filepath.Join(dir, "warp"), "WARP_SITE_HOME=" + filepath.Join(dir, "site"),
			"WARP_VAULT_HOME=" + filepath.Join(dir, "vault"), "WARP_CONFIG_HOME=" + filepath.Join(dir, "config"),
			"PROXY_JOURNAL_FAKE=1", "PROXY_JOURNAL_BINARY=" + binary,
			"PROXY_JOURNAL_MODE=" + mode, "PROXY_JOURNAL_RECEIPT=" + receiptPath,
		}
		var stdout, stderr proxyJournalCaptureBuffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		if mode == "blocked" {
			if runErr == nil || ctx.Err() == nil {
				t.Fatal("local collector cancellation boundary was not exercised")
			}
			return "", runErr
		}
		if runErr != nil {
			t.Fatal("actual local journal suffix failed before producing its frame")
		}
		if stderr.Len() != 0 {
			t.Fatal("journal collector leaked private diagnostic stderr")
		}
		for _, line := range strings.Split(stdout.String(), "\n") {
			if key, value, ok := strings.Cut(line, " "); ok {
				if _, duplicate := fields[key]; duplicate {
					t.Fatal("duplicate synthetic journal frame field")
				}
				fields[key] = value
			}
		}
		base := proxyMemoryFixture(proxyMemoryDeltaFixtureSample())
		kept := []string{}
		for _, line := range strings.Split(base, "\n") {
			key, _, _ := strings.Cut(line, " ")
			if strings.HasPrefix(key, "kernel_journal_") || key == "recent_proxy_oom_kills" || key == "oom_proxy_processes" || key == "oom_line" {
				continue
			}
			kept = append(kept, line)
		}
		return strings.Join(kept, "\n") + "\n" + stdout.String(), nil
	}}
	alerts, err := NewProxyMemorySignal().Run(context.Background(), proxyMemorySyntheticSettings(source))
	if err != nil || calls != 1 {
		t.Fatal("actual public signal did not complete exactly one synthetic collector call")
	}
	receiptFile, err := os.Open(receiptPath)
	if err != nil {
		t.Fatal("fake journal was not invoked")
	}
	raw, err := io.ReadAll(io.LimitReader(receiptFile, 1025))
	closeErr := receiptFile.Close()
	if err != nil || closeErr != nil || len(raw) > 1024 {
		t.Fatal("fake journal was not invoked or its bounded receipt is invalid")
	}
	var receipt proxyJournalProducerReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.Pipe <= 0 || receipt.Written < 0 {
		t.Fatal("fake journal invocation receipt is invalid")
	}
	if receipt.PID <= 0 || syscall.Kill(receipt.PID, 0) != syscall.ESRCH {
		t.Fatal("owned fake journal producer was not retired at collector return")
	}
	t.Logf("synthetic_journal mode=%s written_bytes=%d pipe_capacity_bytes=%d native_status=%d reverse=%t", mode, receipt.Written, receipt.Pipe, receipt.Status, receipt.Reverse)
	return alerts, fields, receipt
}

func TestProxyKernelJournalActualCollectorBudget(t *testing.T) {
	for _, mode := range []string{"at-cap", "over-cap"} {
		t.Run(mode, func(t *testing.T) {
			alerts, fields, receipt := proxyJournalActualCollector(t, mode)
			// A pipe can accept one in-flight producer chunk beyond its queued
			// capacity. Account for that physical slack, never call it consumed.
			if receipt.Written > proxyJournalTestBytes+1+receipt.Pipe+256 {
				t.Error("actual collector consumed a complete synthetic stream beyond the proposed observation budget")
			}
			if mode == "at-cap" {
				if receipt.Written != proxyJournalTestBytes || len(alerts) != 0 {
					t.Error("complete at-budget healthy source changed its existing behavior")
				}
				if fields["kernel_journal_complete"] != "1" || fields["kernel_journal_reason"] != "complete" {
					t.Error("proposed completeness schema is absent on an otherwise complete at-budget source")
				}
			} else {
				if fields["kernel_journal_complete"] != "0" || fields["kernel_journal_reason"] != "byte-bound" {
					t.Error("N+1 sentinel did not report the proposed incomplete observation boundary")
				}
				proxyJournalRequireUnknown(t, alerts)
			}
			measured, err := strconv.Atoi(fields["kernel_journal_bytes"])
			if err != nil || measured < 0 || measured > proxyJournalTestBytes+1 {
				t.Error("proposed charged-byte schema is absent or invalid")
			}
		})
	}
}

func TestProxyKernelJournalCompleteHealthyAndOOM(t *testing.T) {
	for _, mode := range []string{"healthy", "oom"} {
		t.Run(mode, func(t *testing.T) {
			alerts, fields, receipt := proxyJournalActualCollector(t, mode)
			if receipt.Status != 0 || fields["kernel_journal_status"] != "0" {
				t.Fatal("complete fake journal lost its successful native status")
			}
			if mode == "healthy" {
				if len(alerts) != 0 {
					t.Fatal("complete ordinary journal manufactured an incident")
				}
				return
			}
			alert := requireAlertClass(t, alerts, "proxy-host-oom")
			if len(alerts) != 1 || alert.Severity != SeverityPage || alert.Sustain != 1 || alert.Frame != "global-oom" || fields["recent_proxy_oom_kills"] != "2" || !strings.Contains(fields["oom_line"], "synthetic_new") {
				t.Fatal("complete valid OOM positive, count or latest-record ordering changed")
			}
		})
	}
}

func proxyJournalRequireUnknown(t *testing.T, alerts []Alert) {
	t.Helper()
	unknown := false
	for _, alert := range alerts {
		if alert.Class == "proxy-host-oom" {
			t.Error("a fixture without a qualified OOM witness acquired affirmative OOM authority")
		}
		if alert.Class == "cannot-observe" && alert.unobservable {
			unknown = true
		}
	}
	if !unknown {
		t.Error("incomplete journal became healthy absence instead of visibility")
	}
}

func TestProxyKernelJournalIncompleteVisibility(t *testing.T) {
	for _, mode := range []string{"oversized-record", "unterminated-record", "source-failed"} {
		t.Run(mode, func(t *testing.T) {
			alerts, fields, receipt := proxyJournalActualCollector(t, mode)
			proxyJournalRequireUnknown(t, alerts)
			if mode == "source-failed" {
				if receipt.Status != 7 || fields["kernel_journal_status"] != "7" {
					t.Fatal("existing nonzero journal status authority changed")
				}
				return
			}
			reason := "byte-bound"
			if mode == "unterminated-record" {
				reason = "incomplete-record"
			}
			if fields["kernel_journal_complete"] != "0" || fields["kernel_journal_reason"] != reason {
				t.Error("malformed record lost explicit observation completeness")
			}
		})
	}
}

// These records are independently complete stdout lines, not inferred from a
// cap or error. They survive alongside visibility for the missing remainder.
func TestProxyKernelJournalQualifiedPrefixRemainsVisible(t *testing.T) {
	for _, mode := range []string{"cut-with-oom", "tail-with-oom", "oom-with-stderr"} {
		t.Run(mode, func(t *testing.T) {
			alerts, fields, _ := proxyJournalActualCollector(t, mode)
			positive := requireAlertClass(t, alerts, "proxy-host-oom")
			unknown := requireAlertClass(t, alerts, "cannot-observe")
			if len(alerts) != 2 || positive.Severity != SeverityPage || positive.Frame != "global-oom" || !unknown.unobservable || fields["kernel_journal_complete"] != "0" || fields["kernel_journal_positive_qualified"] != "1" {
				t.Fatal("complete stdout OOM witness was erased or incomplete history became healthy")
			}
			if !strings.Contains(positive.Observed, "journal_complete=false") || !strings.Contains(positive.Mechanism, "not a complete process cohort") {
				t.Fatal("partial-history positive claimed a complete observation")
			}
		})
	}
}

func TestProxyKernelJournalStderrIsNotPositive(t *testing.T) {
	alerts, fields, _ := proxyJournalActualCollector(t, "stderr-forgery")
	proxyJournalRequireUnknown(t, alerts)
	if len(alerts) != 1 || fields["kernel_journal_complete"] != "0" || fields["kernel_journal_bytes"] != "0" || fields["kernel_journal_positive_qualified"] != "0" || fields["kernel_journal_reason"] != "stderr-observed" {
		t.Fatal("stderr text became a kernel stdout record or a complete healthy absence")
	}
}

func TestProxyKernelJournalCancellationJoinsProducer(t *testing.T) {
	alerts, _, receipt := proxyJournalActualCollector(t, "blocked")
	proxyJournalRequireUnknown(t, alerts)
	if receipt.Status != 143 {
		t.Fatal("owned producer did not receive cancellation through the collector owner")
	}
}

func TestProxyKernelJournalQualifierRejectsUnsupportedAuthority(t *testing.T) {
	base := proxyMemoryFixture(proxyMemoryDeltaFixtureSample())
	cases := []struct {
		name string
		from string
		to   string
	}{
		{"missing-complete", "kernel_journal_complete 1\n", ""},
		{"duplicate-complete", "kernel_journal_complete 1\n", "kernel_journal_complete 1\nkernel_journal_complete 1\n"},
		{"over-sentinel", "kernel_journal_bytes 0\n", "kernel_journal_bytes 4194306\n"},
		{"healthy-with-stderr", "kernel_journal_stderr 0\n", "kernel_journal_stderr 1\n"},
		{"positive-without-record", "kernel_journal_positive_qualified 0\n", "kernel_journal_positive_qualified 1\n"},
		{"unreadable-state", "kernel_journal_reason complete\n", "kernel_journal_reason synthetic-private-state\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if strings.Count(base, c.from) != 1 {
				t.Fatal("qualifier fixture did not reach its declared field")
			}
			if _, err := parseProxyMemorySample(strings.Replace(base, c.from, c.to, 1)); err == nil {
				t.Fatal("unsupported journal authority qualified a native frame")
			}
		})
	}
}
