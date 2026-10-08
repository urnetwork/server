package connect

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/urnetwork/server"
)

func TestArinCurrentCauseMethodsDoNotOpenColdRecorder(t *testing.T) {
	handler, close, err := newArinShadowConnectRPC(context.Background(), &server.ArinShadowRuntimeConfig{
		ActivePath: "/absent-current-cause-active.mmdb", CandidatePath: "/absent-current-cause-candidate.mmdb",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	value, err := handler(t.Context(), server.ArinCurrentCauseInventoryMethod, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal("cheap inventory opened the cold recorder", err)
	}
	inventory, ok := value.(ArinCurrentCauseHandlerInventory)
	if !ok || inventory.ReaderEpoch < 0 || len(inventory.Handlers) > 256 {
		t.Fatal("unbounded or malformed inventory")
	}
	// Invalid exact-key input refuses before an owner, database or reader call.
	if _, err := handler(t.Context(), server.ArinCurrentCauseMethod, json.RawMessage(`{"expected_epoch":0,"lookup_not_before":"2026-10-06T23:26:04.646404Z","connections":[]}`)); err == nil {
		t.Fatal("invalid current-cause input accepted")
	}
	close()
	if _, err := handler(t.Context(), server.ArinCurrentCauseInventoryMethod, json.RawMessage(`{}`)); err == nil {
		t.Fatal("closed owner accepted inventory")
	}
}
