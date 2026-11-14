// A registration reports an exact retry, so the evidence writer can read the
// already stored wrapper before it writes anything.
package model

import (
	"bytes"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// unchanged is true exactly when the current generation is returned as it is:
// a repeat of the same key, also after a rotation, but not a first
// registration, a rotation or a removal.
func TestStClientKeyRegistrationReportsExactRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		input := newStClientKeyHistoryTestInput(tb)
		register := func(want bool) *StClientKeyHistoryRecord {
			tb.Helper()
			record, unchanged, err := StoreStClientKeyRegistrationReportingRetry(tb.Context(), input)
			if err != nil || record == nil || unchanged != want {
				tb.Fatalf("registration unchanged=%t err=%v, want unchanged=%t", unchanged, err, want)
			}
			return record
		}
		first := register(false)
		input.CreatedAt = input.CreatedAt.Add(time.Hour)
		retry := register(true)
		if !bytes.Equal(retry.EvidenceBytes, first.EvidenceBytes) || retry.EvidenceHash != first.EvidenceHash {
			tb.Fatal("an exact retry returned other evidence")
		}
		input.PublicKey = bytes.Repeat([]byte{10}, 32)
		input.Boundary.Block++
		input.Boundary.Hash = [32]byte{6}
		rotated := register(false)
		if rotated.Registration.Generation != 2 || rotated.EvidenceHash == first.EvidenceHash {
			tb.Fatal("a rotation did not create the next generation")
		}
		if again := register(true); again.EvidenceHash != rotated.EvidenceHash {
			tb.Fatal("a retry after the rotation returned other evidence")
		}
		input.PublicKey = nil
		input.Boundary.Block++
		input.Boundary.Hash = [32]byte{7}
		if removed := register(false); removed.Registration.Present || removed.Registration.Generation != 3 {
			tb.Fatal("a removal did not create its own generation")
		}
		register(true)
		// the original entry point still returns the same records
		record, err := StoreStClientKeyRegistration(tb.Context(), input)
		if err != nil || record.Registration.Generation != 3 {
			tb.Fatalf("StoreStClientKeyRegistration = %v", err)
		}
	})
}

// A refused input reports neither a record nor a retry.
func TestStClientKeyRegistrationReportingRetryRefusesIncompleteInput(t *testing.T) {
	input := newStClientKeyHistoryUnitTestInput(t, server.NewId())
	input.RootKey = nil
	record, unchanged, err := StoreStClientKeyRegistrationReportingRetry(t.Context(), input)
	if err == nil || record != nil || unchanged {
		t.Fatalf("incomplete input: record=%v unchanged=%t err=%v", record, unchanged, err)
	}
}
