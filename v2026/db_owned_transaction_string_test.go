// Canonical queue strings must not collapse to a shortened or untyped identity.
package server

import (
	"strings"
	"testing"
)

func TestPgOwnershipStringKeyKeepsCompleteTypedIdentity(t *testing.T) {
	domain, identity := "pending_task:run_once", `["synthetic-queue","a"]`
	want := PgOwnershipKey{first: 1315811118, second: -97319152}
	if got := NewPgOwnershipKeyFromString(domain, identity); got != want || got != NewPgOwnershipKeyFromString(domain, identity) {
		t.Fatal("canonical string owner changed its stable encoding")
	}
	id := Id{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	idKey := NewPgOwnershipKey("account_balance", id)
	if idKey != (PgOwnershipKey{first: 974475802, second: -60566238}) ||
		idKey == NewPgOwnershipKeyFromString("account_balance", string(id[:])) {
		t.Fatal("string factory changed or reused the existing Id encoding")
	}
	prefix := strings.Repeat("synthetic-queue-prefix", 512)
	inputs := []struct {
		domain   string
		identity string
	}{
		{domain: domain, identity: prefix},
		{domain: domain, identity: prefix + "\x00a"},
		{domain: domain, identity: prefix + "\x00b"},
		{domain: domain, identity: prefix + " "},
		{domain: "pending_task:task_id", identity: prefix},
	}
	seen := map[PgOwnershipKey]bool{}
	for _, input := range inputs {
		key := NewPgOwnershipKeyFromString(input.domain, input.identity)
		if seen[key] {
			t.Fatal("canonical ownership discarded a domain, suffix, zero byte or whitespace")
		}
		seen[key] = true
	}
}

func TestPgOwnershipStringKeyRejectsMissingOrAmbiguousDomain(t *testing.T) {
	for _, input := range []struct {
		domain   string
		identity string
	}{
		{domain: "", identity: "synthetic"},
		{domain: "pending_task\x00run_once", identity: "synthetic"},
		{domain: "pending_task:run_once", identity: ""},
	} {
		if captureDbErrorPanic(func() { _ = NewPgOwnershipKeyFromString(input.domain, input.identity) }) == nil {
			t.Fatal("missing or ambiguous canonical ownership identity was accepted")
		}
	}
}
