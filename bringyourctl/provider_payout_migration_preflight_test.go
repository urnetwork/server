// Migration admission tests use a private synthetic schedule and a counted
// schema callback. They never connect to a database or prepare a real boundary.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

// The synthetic identity uses the production grammar without live accounts.
func payoutMigrationTestSchedule() []byte {
	return []byte(fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: 2035-01-01T00:00:00Z\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", "0x"+strings.Repeat("11", 32)))
}

// An isolated config root makes absent-resource cases independent of host files.
// Nil leaves the resource absent; a nonnil empty slice creates an empty file.
func payoutMigrationTestFile(t *testing.T, data []byte) string {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", directory)
	t.Setenv("WARP_ENV", "local")
	t.Setenv("URNETWORK_ST_PROFILE", "")
	path := filepath.Join(directory, "sn.yml")
	if data != nil {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// Explicit malformed pins, including a present empty option, never fall back
// to ordinary unselected migration or reach the first schema callback.
func TestPayoutMigrationMalformedPinCannotStartSchemaWrites(t *testing.T) {
	data := payoutMigrationTestSchedule()
	payoutMigrationTestFile(t, data)
	for _, pin := range []any{"", "sha256:" + strings.Repeat("12", 32), strings.Repeat("AB", 32), strings.Repeat("0", 64), strings.Repeat("12", 31), strings.Repeat("zz", 32), true} {
		calls := 0
		var output bytes.Buffer
		err := migrateWithPayoutBoundary(t.Context(), docopt.Opts{"--sn-schedule-sha256": pin}, &output, func(context.Context) { calls++ })
		if err == nil || calls != 0 || output.Len() != 0 {
			t.Fatalf("malformed selected pin reached migration: pin=%v calls=%d output=%q err=%v", pin, calls, output.String(), err)
		}
	}
}

// Missing optional-local and malformed present schedules both refuse an
// explicitly selected boundary before migration, without any database read.
func TestPayoutMigrationMissingOrInvalidScheduleCannotStartSchemaWrites(t *testing.T) {
	valid := payoutMigrationTestSchedule()
	digest := sha256.Sum256(valid)
	opts := docopt.Opts{"--sn-schedule-sha256": hex.EncodeToString(digest[:])}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "missing"},
		{name: "empty", data: []byte{}},
		{name: "malformed", data: []byte("schema: [")},
		{name: "invalid-policy", data: bytes.Replace(valid, []byte("settled_contract_close_time"), []byte("unreviewed_attribution"), 1)},
	} {
		payoutMigrationTestFile(t, test.data)
		calls := 0
		var output bytes.Buffer
		err := migrateWithPayoutBoundary(t.Context(), opts, &output, func(context.Context) { calls++ })
		if err == nil || calls != 0 || output.Len() != 0 {
			t.Fatalf("invalid selected schedule reached migration: %s calls=%d output=%q err=%v", test.name, calls, output.String(), err)
		}
	}
}

// A syntactically valid independent pin must match the exact original file.
func TestPayoutMigrationWrongDigestCannotStartSchemaWrites(t *testing.T) {
	payoutMigrationTestFile(t, payoutMigrationTestSchedule())
	calls := 0
	var output bytes.Buffer
	err := migrateWithPayoutBoundary(t.Context(), docopt.Opts{"--sn-schedule-sha256": strings.Repeat("22", 32)}, &output, func(context.Context) { calls++ })
	if err == nil || calls != 0 || output.Len() != 0 {
		t.Fatalf("wrong schedule pin reached migration: calls=%d output=%q err=%v", calls, output.String(), err)
	}
}

// Cancellation and a missing owner context refuse before schema mutation.
func TestPayoutMigrationCanceledOwnerCannotStartSchemaWrites(t *testing.T) {
	data := payoutMigrationTestSchedule()
	payoutMigrationTestFile(t, data)
	digest := sha256.Sum256(data)
	opts := docopt.Opts{"--sn-schedule-sha256": hex.EncodeToString(digest[:])}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	var output bytes.Buffer
	err := migrateWithPayoutBoundary(ctx, opts, &output, func(context.Context) { calls++ })
	if !errors.Is(err, context.Canceled) || calls != 0 || output.Len() != 0 {
		t.Fatal("canceled migration owner reached schema writes", calls, err)
	}
	if err := migrateWithPayoutBoundary(nil, opts, &output, func(context.Context) { calls++ }); err == nil || calls != 0 {
		t.Fatal("missing migration owner reached schema writes", calls, err)
	}
}

// A real parsed schedule admits migration, but even an identity-preserving raw
// byte change during it must still fail the existing insertion-time reload.
func TestPayoutMigrationReloadsScheduleAfterSchemaWrites(t *testing.T) {
	data := payoutMigrationTestSchedule()
	path := payoutMigrationTestFile(t, data)
	digest := sha256.Sum256(data)
	opts := docopt.Opts{"--sn-schedule-sha256": hex.EncodeToString(digest[:])}
	calls := 0
	var output bytes.Buffer
	err := migrateWithPayoutBoundary(t.Context(), opts, &output, func(context.Context) {
		calls++
		if err := os.WriteFile(path, append(bytes.Clone(data), '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil || calls != 1 || output.Len() != 0 {
		t.Fatalf("preflight replaced the insertion-time file check: calls=%d output=%q err=%v", calls, output.String(), err)
	}
}

// Ordinary migration neither requires nor parses an unselected schedule. These
// are the two omitted-option shapes used by direct callers and docopt.
func TestPayoutMigrationOmittedSchedulePreservesOrdinaryMigration(t *testing.T) {
	payoutMigrationTestFile(t, []byte("schema: ["))
	for _, opts := range []docopt.Opts{{}, {"--sn-schedule-sha256": nil}} {
		calls := 0
		var output bytes.Buffer
		err := migrateWithPayoutBoundary(t.Context(), opts, &output, func(context.Context) { calls++ })
		if err != nil || calls != 1 || output.Len() != 0 {
			t.Fatalf("unselected schedule changed ordinary migration: calls=%d output=%q err=%v", calls, output.String(), err)
		}
	}
}
