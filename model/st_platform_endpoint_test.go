// The SN25 extender amendment's platform endpoint kinds without a database (sn
// WHITEPAPER.md §8.6.2, the owner's decision of 2026-11-12): the signed entry
// covers sidecar's TLS, SMTP and MX edges and nothing else, so the registry
// refuses every other kind before it reads anything, and a new kind needs a new
// signed amendment entry, not a registration.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestStPlatformEndpointKindsAreTheSignedSet(t *testing.T) {
	// Changing this set changes which traffic counts toward Z: it needs a new
	// signed amendment entry first.
	if want := []string{"tlsedge", "smtpedge", "mxedge"}; !slices.Equal(stPlatformEndpointKinds, want) {
		t.Fatalf("platform endpoint kinds %v, the signed set is %v", stPlatformEndpointKinds, want)
	}
}

func TestStPlatformEndpointRefusesAKindOutsideTheSignedSetBeforeReadingAnything(t *testing.T) {
	// A cancelled context: a registration that went on to read the database would
	// fail there, with an error that is not the kind's refusal, never reaching a
	// database.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clientId, networkId := server.NewId(), server.NewId()
	// Every network the client could be on is the operator's own, so only the
	// kind can refuse.
	networks := StPlatformEndpointNetworks{OperatorNetworkIds: map[server.Id]bool{networkId: true}, MeasurementNetworkIds: map[server.Id]bool{}}
	for _, kind := range []string{
		"",
		"egress",
		"provider",
		"extender",
		"httpedge",
		"dnsedge",
		"TLSEDGE",
		"TlsEdge",
		" tlsedge",
		"tlsedge ",
		"tls",
		"smtp",
		"mx",
		"tlsedge,smtpedge",
	} {
		registered, err := RegisterStPlatformEndpoint(ctx, clientId, kind, "operator@example", networks)
		if registered != nil || !errors.Is(err, errStPlatformEndpoint) {
			t.Fatalf("kind %q: registered %+v, %v", kind, registered, err)
		}
		if want := fmt.Sprintf("kind %q is not one of", kind); !strings.Contains(err.Error(), want) {
			t.Fatalf("kind %q refused for another reason: %v", kind, err)
		}
	}
}
