// Public pool-retention controls cover overflow-safe conservation and unknown observations.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// Failed aggregates remain unknown and keep private source text out of public formats.
func requirePoolConservationVisibility(t *testing.T, alerts Alerts, err error, forbidden ...string) {
	t.Helper()
	if err == nil || len(alerts) != 1 {
		t.Fatal("invalid pool aggregate became healthy or a measured condition")
	}
	alert := alerts[0]
	if alert.Class != "cannot-observe" || alert.SignalID != "monitor/visibility" ||
		alert.Target != "pg/pool-retention" || alert.Severity != SeverityWarn ||
		alert.Sustain != 2 || !alert.unobservable {
		t.Fatal("pool aggregate failure changed the visibility contract")
	}
	encoded, encodeErr := json.Marshal(alerts)
	if encodeErr != nil {
		t.Fatal("cannot encode visibility JSON")
	}
	var jsonl strings.Builder
	if writeErr := alerts.WriteJSONL(&jsonl); writeErr != nil {
		t.Fatal("cannot encode visibility JSONL")
	}
	for _, public := range []string{alerts.Markdown(), string(encoded), jsonl.String()} {
		for _, marker := range forbidden {
			if strings.Contains(public, marker) {
				t.Fatal("pool aggregate visibility leaked a synthetic private marker")
			}
		}
	}
}

// Two individually valid maximum counts must fail before the public healthy result.
func TestPoolRetentionConservationRejectsWrappedPartition(t *testing.T) {
	maximum := strconv.Itoa(int(^uint(0) >> 1))
	calls := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		calls++
		return poolRetentionRows("1000", "0", "0", "0", maximum, maximum, "0", "0"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil {
		t.Fatal("overflowed pool partition was accepted as healthy")
	}
	if len(alerts) != 0 || calls != 1 {
		t.Fatal("rejected pool aggregate emitted a condition or repeated its source")
	}
}

// Each subtraction is guarded, including excess that appears only in the last component.
func TestPoolRetentionConservationImpossibleRowsRemainUnknown(t *testing.T) {
	maximum := strconv.Itoa(int(^uint(0) >> 1))
	cases := []struct {
		name string
		row  Row
	}{
		{name: "wrapped active and transaction", row: Row{"1000", "0", "0", "0", maximum, maximum, "0", "0"}},
		{name: "wrapped idle and active", row: Row{"1000", "0", "0", maximum, maximum, "0", "0", "0"}},
		{name: "wrapped idle and transaction", row: Row{"1000", "0", "0", maximum, "0", maximum, "0", "0"}},
		{name: "idle exceeds parent", row: Row{"1000", "10", "10", "11", "0", "0", "0", "0"}},
		{name: "active exceeds remainder", row: Row{"1000", "10", "10", "5", "6", "0", "0", "0"}},
		{name: "transaction exceeds remainder", row: Row{"1000", "10", "10", "5", "5", "1", "0", "0"}},
		{name: "local exceeds total", row: Row{"1000", "9", "10", "5", "5", "0", "0", "0"}},
		{name: "aged exceeds idle", row: Row{"1000", "10", "10", "5", "5", "0", "6", "600"}},
	}
	for _, c := range cases {
		calls := 0
		source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
			calls++
			return []Row{c.row}, nil
		}}
		alerts, err := NewWithSignals(syntheticSettings(source), NewPoolRetentionSignal()).Run(context.Background())
		requirePoolConservationVisibility(t, alerts, err)
		if calls != 1 {
			t.Fatalf("%s repeated its source", c.name)
		}
	}
}

// The supplied small healthy control retains its normal public result.
func TestPoolRetentionConservationHealthyControl(t *testing.T) {
	calls := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		calls++
		return poolRetentionRows("1000", "10", "10", "5", "5", "0", "0", "0"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 || calls != 1 {
		t.Fatal("conservation check changed a complete healthy reserve")
	}
}

// An exact maximum partition is representable and must not be rejected by a stricter cap.
func TestPoolRetentionConservationMaximumBoundaryIsValid(t *testing.T) {
	maximum := int(^uint(0) >> 1)
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows(strconv.Itoa(maximum), strconv.Itoa(maximum), strconv.Itoa(maximum),
			"0", strconv.Itoa(maximum-1), "1", "0", "0"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatal("valid maximum partition was rejected")
	}
}

// Unlisted PostgreSQL states may legitimately leave a partial partition.
func TestPoolRetentionConservationAllowsUnlistedStates(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1000", "10", "10", "5", "2", "1", "0", "0"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatal("conservation check required all backend states to be listed")
	}
}

// A numeric cell above the platform integer domain remains unknown, not zero.
func TestPoolRetentionConservationUnrepresentableCellIsUnknown(t *testing.T) {
	outside := strconv.FormatUint(uint64(^uint(0)>>1)+1, 10)
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1000", "10", "10", "0", outside, "0", "0", "0"), nil
	}}
	alerts, err := NewWithSignals(syntheticSettings(source), NewPoolRetentionSignal()).Run(context.Background())
	requirePoolConservationVisibility(t, alerts, err, outside)
}

// Valid high idle counts retain the existing warning identity and ten-run sustain.
func TestPoolRetentionConservationPreservesPressureWarning(t *testing.T) {
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1000", "700", "600", "560", "35", "5", "400", "1700"), nil
	}}
	alerts, err := NewPoolRetentionSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 {
		t.Fatal("valid pool pressure observation was lost")
	}
	alert := alerts[0]
	if alert.Class != "pgbouncer-idle-retention" || alert.SignalID != "pg/pool-retention" ||
		alert.Severity != SeverityWarn || alert.Sustain != 10 || alert.unobservable ||
		!strings.Contains(alert.Observed, "loopback_idle=560") {
		t.Fatal("conservation check changed the pressure contract")
	}
}

// The source cancels its parent before returning; the public run keeps its context cause and unknown status.
func TestPoolRetentionConservationCanceledSourcePreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cause := fmt.Errorf("synthetic-private-pool-cancel: %w", context.Canceled)
	calls := 0
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		calls++
		cancel()
		return poolRetentionRows("1000", "10", "10", "5", "5", "0", "0", "0"), cause
	}}
	alerts, err := NewWithSignals(syntheticSettings(source), NewPoolRetentionSignal()).Run(ctx)
	requirePoolConservationVisibility(t, alerts, err, "synthetic-private-pool-cancel")
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("canceled source lost its cause or was retried")
	}
}

// Malformed cells keep the public error projection private; parser error text has a separate owner.
func TestPoolRetentionConservationMalformedCellKeepsPublicPrivacy(t *testing.T) {
	marker := "synthetic-private-pool-count"
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		return poolRetentionRows("1000", "10", "10", "5", marker, "0", "0", "0"), nil
	}}
	alerts, err := NewWithSignals(syntheticSettings(source), NewPoolRetentionSignal()).Run(context.Background())
	requirePoolConservationVisibility(t, alerts, err, marker)
}
