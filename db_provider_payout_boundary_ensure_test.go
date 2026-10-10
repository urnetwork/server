// Ordinary db migrate's boundary step against a private test database. Every
// schedule is the synthetic transition fixture or a synthetic edit of it.
package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// Counts boundary rows directly, so a retained or absent result cannot hide a
// second row or a row the step should not have written.
func countProviderPayoutBoundaryRows(t testing.TB, ctx context.Context) int {
	t.Helper()
	var count int
	Db(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_payout_boundary`).Scan(&count))
	})
	return count
}

// A missing boundary is prepared from the loaded schedule's own digest and then
// admits workers. A replay retains that row instead of preparing it again.
func TestProviderBoundaryEnsurePreparesMissingFromLoadedSchedule(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		data, policy := boundaryTestPolicy(t)
		t.Cleanup(Config.PushSimpleResource("sn.yml", data))
		identity, err := policy.EarningIdentitySha256()
		if err != nil {
			t.Fatal(err)
		}
		binding, prepared, err := EnsureProviderPayoutBoundary(ctx)
		if err != nil || !prepared || binding == nil || binding.IdentitySha256 != identity || binding.InitialConfigSha256 != policy.ConfigSha256 || binding.CutoffUtc != policy.CutoffUtc {
			t.Fatal("missing boundary was not prepared from the loaded schedule", binding, prepared, err)
		}
		if _, err := LoadProviderPayoutEarningPolicy(ctx); err != nil {
			t.Fatal("prepared boundary did not admit workers", err)
		}
		again, prepared, err := EnsureProviderPayoutBoundary(ctx)
		if err != nil || prepared || again == nil || again.IdentitySha256 != identity || again.InitialConfigSha256 != policy.ConfigSha256 || !again.PreparedAt.Equal(binding.PreparedAt) {
			t.Fatal("replay did not retain the prepared boundary", again, prepared, err)
		}
		if count := countProviderPayoutBoundaryRows(t, ctx); count != 1 {
			t.Fatal("preparation left other than one boundary row", count)
		}
	})
}

// A prepared boundary is never rewritten or rechecked against later schedule
// bytes. A readiness edit, an earning edit, malformed bytes and removal (an
// error under the mainnet profile) all retain the initial row and digest, while
// workers still refuse the edited earning identity on use.
func TestProviderBoundaryEnsureRetainsExistingAcrossScheduleEdits(t *testing.T) {
	t.Setenv("URNETWORK_ST_PROFILE", "mainnet")
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		data, policy := boundaryTestPolicy(t)
		first, err := prepareProviderPayoutBoundary(ctx, policy, policy.ConfigSha256)
		if err != nil || first == nil {
			t.Fatal(err)
		}
		readiness := bytes.Replace(data, []byte(`"activation":"reviewed"`), []byte(`"activation":"blocked"`), 1)
		earning := bytes.Replace(data, []byte(policy.CutoffUtc), []byte("2026-10-07T00:00:00Z"), 1)
		if bytes.Equal(readiness, data) || bytes.Equal(earning, data) {
			t.Fatal("schedule edit fixtures did not change")
		}
		if _, err := LoadProviderPayoutTransition(ctx); err == nil {
			t.Fatal("removed schedule fixture is not a load error")
		}
		for _, edited := range [][]byte{readiness, earning, []byte("schema: ["), nil} {
			release := func() {}
			if edited != nil {
				release = Config.PushSimpleResource("sn.yml", edited)
			}
			binding, prepared, err := EnsureProviderPayoutBoundary(ctx)
			release()
			if err != nil || prepared || binding == nil || binding.IdentitySha256 != first.IdentitySha256 || binding.InitialConfigSha256 != policy.ConfigSha256 || !binding.PreparedAt.Equal(first.PreparedAt) {
				t.Fatalf("schedule edit %q changed or failed the retained boundary: %+v prepared=%v err=%v", edited, binding, prepared, err)
			}
		}
		if count := countProviderPayoutBoundaryRows(t, ctx); count != 1 {
			t.Fatal("retained boundary gained or lost rows", count)
		}
		release := Config.PushSimpleResource("sn.yml", earning)
		_, err = LoadProviderPayoutEarningPolicy(ctx)
		release()
		if !errors.Is(err, ErrProviderEarningBoundaryMismatch) {
			t.Fatal("edited earning identity was admitted against the retained boundary", err)
		}
	})
}

// Local and test environments without sn.yml migrate as before: nothing is
// prepared or reported, and the boundary table stays empty.
func TestProviderBoundaryEnsureWithoutSchedulePreparesNothing(t *testing.T) {
	t.Setenv("URNETWORK_ST_PROFILE", "")
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if policy, err := LoadProviderPayoutTransition(ctx); err != nil || policy != nil {
			t.Fatal("fixture requires an environment without sn.yml", policy, err)
		}
		binding, prepared, err := EnsureProviderPayoutBoundary(ctx)
		if err != nil || prepared || binding != nil {
			t.Fatal("absent schedule prepared or reported a boundary", binding, prepared, err)
		}
		if count := countProviderPayoutBoundaryRows(t, ctx); count != 0 {
			t.Fatal("absent schedule wrote a boundary row", count)
		}
	})
}

// Before any boundary exists, an invalid declared schedule is the loader's
// error, as it is for workers: nothing is prepared or reported.
func TestProviderBoundaryEnsureInvalidScheduleRefusesPreparation(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(Config.PushSimpleResource("sn.yml", []byte("schema: [")))
		binding, prepared, err := EnsureProviderPayoutBoundary(ctx)
		if err == nil || !strings.HasPrefix(err.Error(), "sn.yml:") || prepared || binding != nil {
			t.Fatal("invalid schedule prepared, reported or hid its error", binding, prepared, err)
		}
		if count := countProviderPayoutBoundaryRows(t, ctx); count != 0 {
			t.Fatal("invalid schedule wrote a boundary row", count)
		}
	})
}

// A canceled or missing owner context is refused before any database or file
// read, and reports no boundary.
func TestProviderBoundaryEnsureCanceledOwnerReadsNothing(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	binding, prepared, err := EnsureProviderPayoutBoundary(canceled)
	if !errors.Is(err, context.Canceled) || prepared || binding != nil {
		t.Fatal("canceled owner reached the boundary step", binding, prepared, err)
	}
	binding, prepared, err = EnsureProviderPayoutBoundary(nil)
	if err == nil || prepared || binding != nil {
		t.Fatal("missing owner context reached the boundary step", binding, prepared, err)
	}
}

// Acknowledges one byte less than it was given, without an error.
type boundaryShortWriter struct{}

// Violates the io.Writer contract on purpose to model a truncated confirmation.
func (self boundaryShortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

// The confirmation is one exact line of public digests, cutoff and preparation
// time, and a truncated write is reported rather than acknowledged.
func TestProviderBoundaryConfirmationLine(t *testing.T) {
	binding := &ProviderEarningBoundary{IdentitySha256: strings.Repeat("cd", 32), InitialConfigSha256: strings.Repeat("ab", 32),
		CutoffUtc: "2035-01-01T00:00:00Z", PreparedAt: time.Date(2035, 1, 2, 3, 4, 5, 600000000, time.UTC)}
	for _, c := range []struct {
		prepared bool
		want     string
	}{
		{prepared: true, want: "Prepared provider earning boundary: cutoff_utc=2035-01-01T00:00:00Z identity_sha256=" + strings.Repeat("cd", 32) + " initial_config_sha256=" + strings.Repeat("ab", 32) + " prepared_at=2035-01-02T03:04:05.6Z\n"},
		{prepared: false, want: "Retained provider earning boundary: cutoff_utc=2035-01-01T00:00:00Z identity_sha256=" + strings.Repeat("cd", 32) + " initial_config_sha256=" + strings.Repeat("ab", 32) + " prepared_at=2035-01-02T03:04:05.6Z\n"},
	} {
		var output bytes.Buffer
		if err := WriteProviderEarningBoundary(&output, binding, c.prepared); err != nil || output.String() != c.want {
			t.Errorf("confirmation prepared=%v = %q, %v; want %q", c.prepared, output.String(), err, c.want)
		}
	}
	if err := WriteProviderEarningBoundary(boundaryShortWriter{}, binding, false); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("truncated confirmation was acknowledged", err)
	}
}
