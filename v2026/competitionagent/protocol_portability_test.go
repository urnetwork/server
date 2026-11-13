// A coordinator consumes only the exported authenticated framing boundary.
package competitionagent_test

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/urnetwork/server/v2026/competitionagent"
)

// A real signed document crosses both exported codecs without a native owner.
func TestCompetitionProtocolPortableAuthenticationAndFraming(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	request := competitionagent.Request{Schema: competitionagent.Schema, Operation: "check", HostId: "executor.example"}
	if err := competitionagent.SignRequest(&request, key); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := competitionagent.WriteDocument(&wire, request, competitionagent.DefaultLimits().DocumentBytes); err != nil {
		t.Fatal(err)
	}
	var received competitionagent.Request
	if err := competitionagent.ReadDocument(&wire, &received, competitionagent.DefaultLimits().DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if received != request || wire.Len() != 0 || competitionagent.VerifyRequest(received, key.Public().(ed25519.PublicKey)) != nil {
		t.Fatal("portable signed protocol changed identity, framing or authentication")
	}
	received.HostId = "other-executor.example"
	if err := competitionagent.VerifyRequest(received, key.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("portable protocol authenticated another executor")
	}
}
