package server

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// scrubThrough runs input through the line scrubber and returns what it wrote.
func scrubThrough(t *testing.T, input string) string {
	t.Helper()
	var out bytes.Buffer
	scrubLoop(strings.NewReader(input), &out)
	return out.String()
}

func TestScrubLoopRemovesAddresses(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"the tls handshake line the gap was about": {
			in:   "http: TLS handshake error from 203.0.113.9:51423: EOF\n",
			want: "http: TLS handshake error from [scrubbed]:51423: EOF\n",
		},
		"ipv4 alone": {
			in:   "dialed 198.51.100.7 ok\n",
			want: "dialed [scrubbed] ok\n",
		},
		"ipv6": {
			in:   "peer 2606:4700:4700::1111 connected\n",
			want: "peer [scrubbed] connected\n",
		},
		"ipv6 with port in brackets": {
			in:   "peer [2606:4700:4700::1111]:443 connected\n",
			want: "peer [scrubbed]:443 connected\n",
		},
		"several on one line": {
			in:   "from 203.0.113.9 to 198.51.100.7\n",
			want: "from [scrubbed] to [scrubbed]\n",
		},
	}
	for name, test := range cases {
		if got := scrubThrough(t, test.in); got != test.want {
			t.Errorf("%s\n got %q\nwant %q", name, got, test.want)
		}
	}
}

// Text that merely looks address-shaped must survive, or the logs stop being
// readable.
func TestScrubLoopKeepsNonAddressText(t *testing.T) {
	for _, line := range []string{
		"started in 1.234s\n",
		"served 0/0 requests\n",
		"at 15:42:35 the worker woke\n",
	} {
		if got := scrubThrough(t, line); got != line {
			t.Errorf("rewrote non-address text\n got %q\nwant %q", got, line)
		}
	}
}

// Lines are only forwarded once complete, so an address split across two writes
// is still matched whole.
func TestScrubLoopHoldsPartialLines(t *testing.T) {
	reader, writer := io.Pipe()
	var out bytes.Buffer
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		scrubLoop(reader, writerFunc(func(b []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return out.Write(b)
		}))
	}()

	writer.Write([]byte("peer 203.0."))
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	partial := out.String()
	mu.Unlock()
	if partial != "" {
		t.Fatalf("forwarded an incomplete line: %q", partial)
	}

	writer.Write([]byte("113.9 done\n"))
	writer.Close()
	<-done

	if got := out.String(); got != "peer [scrubbed] done\n" {
		t.Fatalf("got %q", got)
	}
}

// A writer that never emits a newline must not grow the buffer without bound.
func TestScrubLoopBoundsAnUnterminatedLine(t *testing.T) {
	var out bytes.Buffer
	scrubLoop(strings.NewReader(strings.Repeat("x", scrubMaxLineSize+16)), &out)
	if out.Len() < scrubMaxLineSize {
		t.Fatalf("held %d bytes without flushing, bound is %d", out.Len(), scrubMaxLineSize)
	}
}

// A crash dump is rare and is the most valuable thing in the log when it
// happens, so it passes through untouched.
func TestScrubLoopPassesCrashDumpsThrough(t *testing.T) {
	dump := "panic: runtime error: invalid memory address\n" +
		"\n" +
		"goroutine 42 [running]:\n" +
		"main.handle(0xc000123456, 203.0.113.9)\n"
	got := scrubThrough(t, dump)

	if !strings.Contains(got, "203.0.113.9") {
		t.Fatal("scrubbed a crash dump")
	}
	// and the transition is announced rather than silent
	if !strings.Contains(got, "[scrub] crash output detected") {
		t.Fatal("disabled scrubbing without saying so")
	}
}

// Everything before the dump is still scrubbed.
func TestScrubLoopScrubsUpToTheDump(t *testing.T) {
	got := scrubThrough(t,
		"serving 203.0.113.9\n"+
			"panic: boom\n"+
			"goroutine 1 [running]:\n"+
			"main.f(198.51.100.7)\n")

	if strings.Contains(got, "203.0.113.9") {
		t.Error("a line before the dump was not scrubbed")
	}
	if !strings.Contains(got, "198.51.100.7") {
		t.Error("a line inside the dump was scrubbed")
	}
}

// The end-to-end property: after ScrubProcessLogs, a write to the real stderr
// descriptor comes out scrubbed.
func TestScrubProcessLogsRedirectsTheDescriptor(t *testing.T) {
	captureRead, captureWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %s", err)
	}
	defer captureRead.Close()

	// stand in for the terminal: point stderr at something we can read
	realStderr := os.Stderr
	savedFd, err := dupFd(2)
	if err != nil {
		t.Fatalf("dup: %s", err)
	}
	if err := dup2(int(captureWrite.Fd()), 2); err != nil {
		t.Fatalf("redirect: %s", err)
	}
	defer func() {
		dup2(savedFd, 2)
		closeFd(savedFd)
		os.Stderr = realStderr
	}()

	restore, err := ScrubProcessLogs()
	if err != nil {
		t.Fatalf("ScrubProcessLogs: %s", err)
	}

	os.Stderr.Write([]byte("http: TLS handshake error from 203.0.113.9:51423: EOF\n"))
	time.Sleep(100 * time.Millisecond)
	restore()
	captureWrite.Close()

	got := make([]byte, 4096)
	n, _ := captureRead.Read(got)
	line := string(got[:n])
	if strings.Contains(line, "203.0.113.9") {
		t.Fatalf("the address reached the descriptor: %q", line)
	}
	if !strings.Contains(line, "[scrubbed]:51423") {
		t.Fatalf("expected a scrubbed address, got %q", line)
	}
}

type writerFunc func([]byte) (int, error)

func (self writerFunc) Write(b []byte) (int, error) { return self(b) }
