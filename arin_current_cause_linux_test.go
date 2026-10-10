//go:build linux

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"testing"
)

// The deployed Unix capture transport and its socket fixture are Linux-only.
// Keep decoder, ownership, and aggregation coverage in the portable test file.
func TestArinCurrentCauseAuthenticatedWireAndNoColdReaders(t *testing.T) {
	request, owner, fact, cause, _ := currentCauseFixture(t)
	owners := func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) {
		return []ArinShadowCaptureTarget{{request.Connections[0], owner}}, nil
	}
	facts := func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
		fact.ObservedAt = NowUtc()
		return []ArinShadowCaptureFacts{fact}, nil
	}
	key, run, identity := [32]byte{19}, NewId(), shadowRPCTestIdentity()
	service, err := NewArinShadowRPCService(t.Context(), run, key, identity, func(ctx context.Context, method string, input json.RawMessage) (any, error) {
		if method != ArinCurrentCauseMethod {
			t.Fatal("wrong method")
		}
		var got ArinCurrentCauseRequest
		if DecodeArinShadowRPC(input, &got) != nil {
			return nil, ErrArinShadowInput
		}
		return readCurrentArinCauses(ctx, got, owners, facts, func(netip.Addr) (*ArinCurrentCause, error) { return cause, nil }, NowUtc)
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewArinShadowRPCClient(run, key, identity, shadowRPCTestSocket(t, service))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := CallArinCurrentCauses(t.Context(), client, request)
	if err != nil || len(reply.Rows) != 1 || reply.Rows[0].Reason != "qualified" || reply.Rows[0].Cause == nil {
		t.Fatal("authenticated point capture failed", err)
	}
	// Legacy capture remains its existing strict schema, with no added field.
	legacy, _ := json.Marshal(ArinShadowOrigin{ASNs: []uint32{12345}, UseState: "withheld"})
	if bytes.Contains(legacy, []byte("withheld_reason")) {
		t.Fatal("new method changed legacy capture wire")
	}
}
