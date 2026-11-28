// Exercises Redis dial failure metrics using injected results and synthetic routes.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Exercise the pool's dial entry point with two possible listeners on the
// same port, missing announcements, a tunnel and malformed input. Each call
// must preserve the selected address, context, result and number of attempts.
func TestRedisDialFailureObservesSelectedRouteAndReturnedError(t *testing.T) {
	// the production pool dialer: a service's guard-free dial is its own
	testEnvGuardlessRefusalForTest(t, false)
	refusal := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	timeout := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
	wrappedDeadline := &net.OpError{Op: "dial", Err: fmt.Errorf("wrapped: %w", os.ErrDeadlineExceeded)}
	textOnly := errors.New("dial tcp private.example:12345: i/o timeout")
	const authority = "192.0.2.16:6379"
	routes := map[string]string{"198.51.100.15": "203.0.113.15"}
	for _, tc := range []struct {
		authority, requested, effective, route, reason string
		err                                            error
	}{
		{authority, authority, authority, "authority_endpoint", "refused", refusal},
		{authority, "192.0.2.16:6418", "192.0.2.16:6418", "authority_host_other_port", "timeout", timeout},
		{authority, "198.51.100.15:6418", "203.0.113.15:6418", "other_host", "timeout", fmt.Errorf("wrapped: %w", timeout)},
		{authority, authority, authority, "authority_endpoint", "timeout", wrappedDeadline},
		{authority, "?:6418", "192.0.2.16:6418", "missing_host_fallback", "canceled", context.Canceled},
		{authority, ":6418", "192.0.2.16:6418", "missing_host_fallback", "timeout", context.DeadlineExceeded},
		{"[::1]:6379", "[2001:db8::5]:6418", "[::1]:6418", "loopback_override", "refused", refusal},
		{authority, "private.example:12345", "private.example:12345", "other_host", "other", textOnly},
		{authority, "no-port", "no-port", "unknown", "other", textOnly},
	} {
		counter := redisDialFailures.WithLabelValues(tc.route, tc.reason)
		before := testutil.ToFloat64(counter)
		calls := 0
		// An independently canceled context must not change the returned
		// reason when the supplied dialer reports something else.
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		dial := redisPoolDialer(nil, tc.authority, routes, func(gotCtx context.Context, network, addr string) (net.Conn, error) {
			calls++
			if gotCtx != ctx || network != "tcp" || addr != tc.effective {
				t.Fatalf("%s: dial arguments changed, network=%q address=%q", tc.route, network, addr)
			}
			return nil, tc.err
		})
		conn, err := dial(ctx, "tcp", tc.requested)
		if calls != 1 || conn != nil || err != tc.err {
			t.Fatalf("%s: calls=%d conn=%v err=%v", tc.route, calls, conn, err)
		}
		if delta := testutil.ToFloat64(counter) - before; delta != 1 {
			t.Fatalf("%s/%s increased by %v, want 1", tc.route, tc.reason, delta)
		}
	}
}

// A later successful retry returns its original connection. Only the actual
// failed dial is counted, even when both calls use the same context and port.
func TestRedisDialFailureCountsAttemptsWithoutCountingRecovery(t *testing.T) {
	// the production pool dialer: a service's guard-free dial is its own
	testEnvGuardlessRefusalForTest(t, false)
	registry := prometheus.NewRegistry()
	registry.MustRegister(redisDialFailures)
	total := func() float64 {
		families, err := registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		var count float64
		for _, family := range families {
			for _, sample := range family.GetMetric() {
				count += sample.GetCounter().GetValue()
			}
		}
		return count
	}
	const authority = "192.0.2.16:6379"
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	var calls int
	dial := redisPoolDialer(nil, authority, nil, func(context.Context, string, string) (net.Conn, error) {
		calls++
		if calls == 1 {
			return nil, context.DeadlineExceeded
		}
		return conn, nil
	})
	before := total()
	if _, err := dial(t.Context(), "tcp", authority); err != context.DeadlineExceeded {
		t.Fatalf("first dial returned %v", err)
	}
	if returned, err := dial(t.Context(), "tcp", authority); returned != conn || err != nil {
		t.Fatalf("second dial returned %v, %v", returned, err)
	}
	if delta := total() - before; calls != 2 || delta != 1 {
		t.Fatalf("%d calls counted %v failures", calls, delta)
	}
}
