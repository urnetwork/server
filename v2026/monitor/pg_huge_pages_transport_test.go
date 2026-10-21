// Exercise the production SSH-to-CSV PostgreSQL reader with local byte
// receipts. No SSH, psql or database process is launched by these controls.
package monitor

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Forward the public source seam to the real runner, preserving its context
// and shared strict CSV decoder instead of returning already-decoded rows.
type pgHugePagesNativeTestSource struct {
	*syntheticSource
	native *runner
}

func (self *pgHugePagesNativeTestSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	rows, err := self.native.pg(ctx, query)
	if err != nil {
		return nil, err
	}
	result := make([]Row, len(rows))
	for i, row := range rows {
		result[i] = Row(row)
	}
	return result, nil
}

// Only the last native execution boundary is replaced. The real reader
// builds the authenticated psql command, stdin and CSV receipt before decode.
func pgHugePagesNativeTestSettings(t *testing.T, stdout func(string) string) SignalSettings {
	t.Helper()
	const password = "private-pg-password-canary"
	cfg := &monitorConfig{
		env: "synthetic", addressMode: addressModeOverlay,
		hosts:  []*host{{name: "pg-1", overlayIp: "192.0.2.10", roles: []string{"pg-primary"}}},
		pgPort: 5432, pgUser: "monitor", pgDb: "synthetic", pgPassword: password,
		sshConnectTimeout: time.Second, commandTimeout: time.Second,
	}
	native := newRunner(cfg)
	native.runSSH = func(_ context.Context, args []string, stdin string) (string, string, error) {
		if len(args) < 2 {
			t.Fatal("native primary reader omitted SSH command")
		}
		remote := args[len(args)-1]
		if !strings.Contains(remote, " --csv -t ") || !strings.Contains(remote, "-h localhost") || !strings.Contains(remote, "ON_ERROR_STOP=1") || !strings.Contains(remote, "statement_timeout=30000") || !strings.Contains(remote, "default_transaction_read_only=on") || strings.Contains(strings.Join(args, " "), password) {
			t.Fatal("native primary reader lost bounded CSV or credential contract")
		}
		line, query, found := strings.Cut(stdin, "\n")
		if !found || line != password || query != pgHugePagesQuery {
			t.Fatal("native primary read changed its one-invocation SQL receipt")
		}
		return stdout(query), "", nil
	}
	return syntheticSettings(&pgHugePagesNativeTestSource{syntheticSource: &syntheticSource{}, native: native})
}

// The fake host recognizes both exact receipt layouts. On the sealed old
// query it returns the old one/three-cell output, which the actual CSV reader
// rejects before the off/on assertions. That is the HJ001 causal RED overlay.
func TestPgHugePagesNativeTransportOnOff(t *testing.T) {
	for _, status := range []string{"off", "on"} {
		t.Logf("case: %s", status)
		calls := 0
		settings := pgHugePagesNativeTestSettings(t, func(query string) string {
			calls++
			stamp := syntheticSettings(nil).Now().Unix()
			if strings.HasPrefix(query, "\\! hostname -s\n") {
				return fmt.Sprintf("pg-1\n%d,primary,%s\n", stamp, status)
			}
			if !strings.HasPrefix(query, "\\set monitor_primary_hostname `hostname -s`\nSELECT :'monitor_primary_hostname',") {
				t.Fatalf("%s: new primary receipt did not quote the same-invocation host into its SQL row", status)
			}
			return fmt.Sprintf("pg-1,%d,primary,%s\n", stamp, status)
		})
		alerts, err := NewPgHugePagesSignal().Run(context.Background(), settings)
		if err != nil || calls != 1 {
			t.Fatalf("%s: native primary reader did not finish exactly one bounded read", status)
		}
		if status == "on" {
			if len(alerts) != 0 {
				t.Fatalf("%s: native supported-on receipt did not certify setting health", status)
			}
		} else {
			if len(alerts) != 1 {
				t.Fatalf("%s: native supported-off receipt did not produce one incident", status)
			}
			requireAlertClass(t, alerts, "pg-huge-pages-disabled")
		}
	}
}

// Retain the exact old mixed-width counterexample at the real reader boundary;
// relaxing the shared parser would hide the owning receipt defect.
func TestPgHugePagesNativeOldReceiptRejected(t *testing.T) {
	settings := pgHugePagesNativeTestSettings(t, func(string) string {
		return fmt.Sprintf("pg-1\n%d,primary,off\n", syntheticSettings(nil).Now().Unix())
	})
	rows, err := settings.Source.PostgreSQL(context.Background(), pgHugePagesQuery)
	if !errors.Is(err, csv.ErrFieldCount) || len(rows) != 0 {
		t.Fatal("old mixed-width primary receipt no longer reaches the shared strict CSV rejection")
	}
}

// Partial, malformed, duplicate and mismatched raw receipts remain unknown
// through both the actual transport decoder and the public finding reducer.
func TestPgHugePagesNativeTransportUnknown(t *testing.T) {
	now := syntheticSettings(nil).Now()
	stamp := now.Unix()
	for _, test := range []struct{ name, raw string }{
		{name: "missing", raw: ""},
		{name: "wrong host", raw: fmt.Sprintf("private-source-canary,%d,primary,off\n", stamp)},
		{name: "replica", raw: fmt.Sprintf("pg-1,%d,replica,off\n", stamp)},
		{name: "unsupported", raw: fmt.Sprintf("pg-1,%d,primary,unavailable\n", stamp)},
		{name: "missing setting", raw: fmt.Sprintf("pg-1,%d,primary,\n", stamp)},
		{name: "private setting", raw: fmt.Sprintf("pg-1,%d,primary,private-source-canary\n", stamp)},
		{name: "stale", raw: fmt.Sprintf("pg-1,%d,primary,off\n", now.Add(-31*time.Second).Unix())},
		{name: "future", raw: fmt.Sprintf("pg-1,%d,primary,off\n", now.Add(31*time.Second).Unix())},
		{name: "truncated", raw: fmt.Sprintf("pg-1,%d,primary,off", stamp)},
		{name: "malformed", raw: "\"private-source-canary\n"},
		{name: "partial row", raw: fmt.Sprintf("pg-1,%d,primary\n", stamp)},
		{name: "duplicate", raw: strings.Repeat(fmt.Sprintf("pg-1,%d,primary,off\n", stamp), 2)},
	} {
		t.Logf("case: %s", test.name)
		settings := pgHugePagesNativeTestSettings(t, func(string) string { return test.raw })
		alerts, err := NewPgHugePagesSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: invalid raw primary receipt lost its fixed visibility finding", test.name)
		}
		unknown := requireAlertClass(t, alerts, "pg-huge-pages-unobservable")
		if !unknown.unobservable {
			t.Fatalf("%s: invalid raw primary receipt could clear the prior incident", test.name)
		}
		requireAlertOmits(t, unknown, "private-source-canary", "private-pg-password-canary")
	}
}
