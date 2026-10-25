// Public pool-retention controls keep source failures private while preserving lifecycle and scope.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Public errors and every retained cause must omit private source material.
func requirePoolRetentionPrivateError(t *testing.T, alerts Alerts, err error, private string) {
	t.Helper()
	if len(alerts) != 0 || err == nil {
		t.Fatal("unavailable pool evidence must return an error without findings")
	}
	pending := []error{err}
	for visited := 0; len(pending) != 0; visited++ {
		if visited >= 32 {
			t.Fatal("unexpected error-cause depth")
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			continue
		}
		for _, rendered := range []string{current.Error(), fmt.Sprintf("%v", current), fmt.Sprintf("%+v", current), fmt.Sprintf("%#v", current)} {
			if strings.Contains(rendered, private) {
				t.Fatal("pool-retention error or cause retained private source material")
			}
		}
		switch value := current.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, value.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, value.Unwrap())
		}
	}
}

// Every aggregate column uses the same owned vocabulary, including parse bounds.
func TestPoolRetentionPublicMalformedAggregatePrivacy(t *testing.T) {
	const private = "POOL_RETENTION_PRIVATE_CELL_CANARY"
	for column := 0; column < 8; column++ {
		for _, malformed := range []string{private + "\n\t\"", "-1", strings.Repeat("9", 128)} {
			values := []string{"1021", "360", "270", "250", "12", "8", "0", "590"}
			values[column] = malformed
			source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
				return poolRetentionRows(values...), nil
			}}
			alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
			requirePoolRetentionPrivateError(t, alerts, err, private)
			if !strings.HasPrefix(err.Error(), "invalid PostgreSQL pool-retention ") {
				t.Fatal("malformed aggregate lost its owned error class")
			}
			if strings.Contains(err.Error(), malformed) {
				t.Fatal("malformed aggregate value escaped into a public error")
			}
		}
	}
}

// Direct source failures retain only fixed text and allowlisted lifecycle causes.
func TestPoolRetentionPublicSourceErrorPrivacy(t *testing.T) {
	const private = "POOL_RETENTION_PRIVATE_SOURCE_CANARY"
	for _, test := range []struct {
		name  string
		err   error
		cause error
	}{
		{name: "direct", err: errors.New(private)},
		{name: "mixed-scope-source", err: errors.Join(&hostScopeExcludedError{}, errors.New(private))},
		{name: "wrapped-canceled", err: fmt.Errorf("%s: %w", private, context.Canceled), cause: context.Canceled},
		{name: "wrapped-deadline", err: fmt.Errorf("%s: %w", private, context.DeadlineExceeded), cause: context.DeadlineExceeded},
		{name: "joined-canceled", err: errors.Join(errors.New(private), context.Canceled), cause: context.Canceled},
	} {
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, test.err }}
		alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
		requirePoolRetentionPrivateError(t, alerts, err, private)
		if !strings.HasPrefix(err.Error(), "PostgreSQL pool-retention observation unavailable") {
			t.Fatal("source failure lost its unavailable error class")
		}
		if test.cause != nil && !errors.Is(err, test.cause) {
			t.Fatal("source lifecycle cause was lost")
		}
		if errors.Is(err, test.err) {
			t.Fatal("private original source error remains in the public cause chain")
		}
	}
}

// Adapter cancellation must not reintroduce a private source cause.
func TestPoolRetentionPublicInterruptedContextPrivacy(t *testing.T) {
	const private = "POOL_RETENTION_PRIVATE_CONTEXT_CANARY"
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		var expected error = context.Canceled
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
			expected = context.DeadlineExceeded
		}
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			cancel()
			return nil, fmt.Errorf("%s: %w", private, ctx.Err())
		}}
		alerts, err := NewPoolRetentionSignal().Run(ctx, syntheticSettings(source))
		cancel()
		requirePoolRetentionPrivateError(t, alerts, err, private)
		if !errors.Is(err, expected) {
			t.Fatal("public interrupted run lost its context cause")
		}
	}
}

// Intentional policy remains incomplete scope and never contacts its source.
func TestPoolRetentionPublicPrivacyPreservesScopePartial(t *testing.T) {
	calls := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		calls++
		return nil, errors.New("POOL_RETENTION_PRIVATE_BLOCKED_CANARY")
	}}
	settings := syntheticSettings(source)
	settings.ExcludedHosts = []string{"pg-1"}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), settings)
	if err != nil || calls != 0 || len(alerts) != 1 || alerts[0].Class != "monitor-host-scope-partial" {
		t.Fatal("pool-retention privacy changed intentional partial-scope semantics")
	}
}

// Inconsistent numeric summaries are unavailable without dumping invalid state.
func TestPoolRetentionPublicInconsistentSummaryPrivacy(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1021", "300", "200", "190", "20", "1", "0", "10"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	requirePoolRetentionPrivateError(t, alerts, err, "normalCeiling:")
	if err.Error() != "inconsistent PostgreSQL pool-retention summary" {
		t.Fatal("inconsistent summary did not use fixed owned text")
	}
}
