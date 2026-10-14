// Deterministic accounting controls keep optional-cache handling financially strict.
package work

import "testing"

// Mirrors reject encodings the actual admission script would refuse.
func TestPrivateLoadAccountingCanonicalDecimals(t *testing.T) {
	for _, value := range []string{"0", "7", "9223372036854775807"} {
		if _, err := privateLoadParseReservation(value); err != nil {
			t.Fatal("canonical reservation rejected")
		}
	}
	for _, value := range []string{"", "-0", "-1", "+7", "07", " 7", "7 ", "9223372036854775808"} {
		if _, err := privateLoadParseReservation(value); err == nil {
			t.Fatal("noncanonical reservation accepted")
		}
	}
}

// A real native reservation is valid without either legacy metadata row.
func TestPrivateLoadAccountingWithoutLegacyCache(t *testing.T) {
	state := privateLoadAccounting{exact: 7, native: 7, promised: 7, remaining: 100, start: 100, nativeMirror: 7, nativeTokens: 7}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
}

// Pure legacy state continues to qualify its current snapshot and mirror.
func TestPrivateLoadAccountingLegacyCache(t *testing.T) {
	state := privateLoadAccounting{exact: 7, legacy: 7, promised: 7, remaining: 100, start: 100, cached: 7, cachePresent: true, cacheCurrent: true, legacyMirror: 7}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
}

// A current legacy snapshot excludes the native partition of the same balance.
func TestPrivateLoadAccountingMixedPartitions(t *testing.T) {
	state := privateLoadAccounting{exact: 12, legacy: 7, native: 5, promised: 12, remaining: 100, start: 100, cached: 7, cachePresent: true, cacheCurrent: true, legacyMirror: 7, nativeMirror: 5, nativeTokens: 5}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
}

// Stale cache contents cannot masquerade as current authority.
func TestPrivateLoadAccountingStaleCache(t *testing.T) {
	state := privateLoadAccounting{exact: 12, legacy: 7, native: 5, promised: 12, remaining: 100, start: 100, cached: 99, cachePresent: true, legacyMirror: 7, nativeMirror: 5, nativeTokens: 5}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
}

// Every mutated authority is rejected even when unrelated cache rows are absent.
func TestPrivateLoadAccountingRejectsCorruption(t *testing.T) {
	for _, mutate := range []func(*privateLoadAccounting){
		func(s *privateLoadAccounting) { s.promised++ },
		func(s *privateLoadAccounting) { s.exact-- },
		func(s *privateLoadAccounting) { s.remaining = 11; s.start = 11 },
		func(s *privateLoadAccounting) { s.remaining-- },
		func(s *privateLoadAccounting) { s.nativeMirror-- },
		func(s *privateLoadAccounting) { s.nativeTokens-- },
		func(s *privateLoadAccounting) { s.legacyMirror-- },
		func(s *privateLoadAccounting) { s.cachePresent = true; s.cacheCurrent = true; s.cached = 12 },
	} {
		state := privateLoadAccounting{exact: 12, legacy: 7, native: 5, promised: 12, remaining: 100, start: 100, legacyMirror: 7, nativeMirror: 5, nativeTokens: 5}
		mutate(&state)
		if state.validate() == nil {
			t.Fatal("corrupt financial authority qualified")
		}
	}
}
