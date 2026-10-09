// Persisted financial RunPost results survive removal of index diagnostics.
package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// An older result retains the continuation fields used by the nil-dispatch Post.
// Removed diagnostic fields are ignored, and new results do not fabricate them.
func TestLegacySettlementResultIgnoresRetiredIndexReadiness(t *testing.T) {
	instant := time.Unix(100, 0).UTC()
	cursor := &model.LegacySettlementCursor{NextAttemptTime: instant, ContractId: server.NewId(), PassEndTime: instant}
	payerCursor := &model.LegacySettlementPayerCursor{End: server.NewId(), PassEndTime: instant}
	serialized, err := json.Marshal(map[string]any{
		"cursor": cursor, "payer_cursor": payerCursor, "completed": 1, "more": true,
		"index_readiness": map[string]any{"outcome": "deadline", "elapsed_ms": 250},
	})
	if err != nil {
		t.Fatal(err)
	}
	var result FlushLegacySettlementsResult
	if err := json.Unmarshal(serialized, &result); err != nil {
		t.Fatal(err)
	}
	if result.Dispatch != nil || result.Completed != 1 || !result.More || result.Cursor == nil ||
		result.Cursor.ContractId != cursor.ContractId || !result.Cursor.NextAttemptTime.Equal(instant) ||
		!result.Cursor.PassEndTime.Equal(instant) || result.PayerCursor == nil ||
		result.PayerCursor.End != payerCursor.End || !result.PayerCursor.PassEndTime.Equal(instant) {
		t.Fatal("retired readiness changed the persisted financial continuation", result)
	}
	serialized, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(serialized, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["index_readiness"]; exists {
		t.Fatal("result fabricated a retired readiness observation")
	}
}
