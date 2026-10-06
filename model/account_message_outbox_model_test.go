// The outbox's stored key and error bounds (no database).
package model

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The stored key is a fixed-length digest of the template and the writer's key,
// so one writer key under two templates owes two messages and no key content is
// stored.
func TestAccountMessageKeyIsScopedByTemplate(t *testing.T) {
	key := "synthetic-network/synthetic-purchase-token/1700000000"
	ended := AccountMessageKey("subscription_ended", key)
	if ended != AccountMessageKey("subscription_ended", key) {
		t.Fatal("the stored key is not deterministic")
	}
	if len(ended) != 64 || strings.Contains(ended, "synthetic") {
		t.Fatalf("stored key = %q, want a 64-character digest", ended)
	}
	if ended == AccountMessageKey("network_welcome", key) {
		t.Fatal("one writer key under two templates has one stored key")
	}
	// the separator keeps a template and key split from colliding with another
	if AccountMessageKey("ab", "c") == AccountMessageKey("a", "bc") {
		t.Fatal("a template and key split collides with another split")
	}
}

// A stored error fits its column without cutting a character.
func TestBoundedAccountMessageErrorKeepsWholeCharacters(t *testing.T) {
	if short := boundedAccountMessageError("synthetic send failure"); short != "synthetic send failure" {
		t.Fatalf("short error = %q, want it whole", short)
	}
	long := strings.Repeat("a", accountMessageLastErrorMaxLength-1) + "é" + strings.Repeat("b", 100)
	bounded := boundedAccountMessageError(long)
	if accountMessageLastErrorMaxLength < len(bounded) || !utf8.ValidString(bounded) {
		t.Fatalf("bounded error is %d bytes, valid utf-8 %t", len(bounded), utf8.ValidString(bounded))
	}
	if bounded != strings.Repeat("a", accountMessageLastErrorMaxLength-1) {
		t.Fatalf("bounded error ends with %q, want the character before the cut", bounded[len(bounded)-4:])
	}
}
