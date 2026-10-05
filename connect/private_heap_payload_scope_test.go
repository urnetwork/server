package connect

import "testing"

func TestPrivateHeapPayloadScopeDefaultOff(t *testing.T) {
	t.Setenv("WARP_HOST", "by-us-fmt-5-edge-3")
	t.Setenv("WARP_BLOCK", "g2")
	for _, options := range []RunOptions{{}, {MemoryOwnerLedger: true}} {
		if exchangeSettingsForRun(options).SDKPayloadOwnerLedger != nil {
			t.Fatal("default or fleet owner flag enabled SDK payload accounting")
		}
	}
}

func TestPrivateHeapPayloadScopeExactTargetOnly(t *testing.T) {
	t.Setenv("WARP_HOST", "by-us-fmt-5-edge-3")
	t.Setenv("WARP_BLOCK", "g2")
	settings := exchangeSettingsForRun(RunOptions{PrivateHeapProfileTarget: "by-us-fmt-5-edge-3/g2"})
	if settings.SDKPayloadOwnerLedger == nil {
		t.Fatal("exact diagnostic target has no accounting scope")
	}
	snapshot := settings.SDKPayloadOwnerLedger.Snapshot()
	if !snapshot.Enabled || !snapshot.Complete || snapshot.SendAck.Owners != 0 || snapshot.Forward.Owners != 0 {
		t.Fatal("fresh target accounting is unavailable or contains borrowed owners")
	}
	if settings.MemoryOwnerLedger != nil || settings.payloadOwnerLedger != nil {
		t.Fatal("private SDK opt-in altered independent resident/memory flags")
	}
}

func TestPrivateHeapPayloadScopeRejectsOtherInstances(t *testing.T) {
	for _, row := range []struct{ host, block, target string }{
		{"by-us-fmt-5-edge-0", "g2", "by-us-fmt-5-edge-3/g2"},
		{"by-us-fmt-5-edge-3", "g1", "by-us-fmt-5-edge-3/g2"},
		{"by-us-fmt-5-edge-3", "", "by-us-fmt-5-edge-3/g2"},
		{"by-us-fmt-5-edge-5", "g2", "by-us-fmt-5-edge-5/g2"},
		{"by-us-fmt-5-edge-3", "beta", "by-us-fmt-5-edge-3/beta"},
		{"by-us-fmt-5-edge-3", "g2", "by-us-fmt-5-edge-3/g2/extra"},
	} {
		t.Run(row.host+"_"+row.block+"_"+row.target, func(t *testing.T) {
			t.Setenv("WARP_HOST", row.host)
			t.Setenv("WARP_BLOCK", row.block)
			if exchangeSettingsForRun(RunOptions{PrivateHeapProfileTarget: row.target}).SDKPayloadOwnerLedger != nil {
				t.Fatal("non-target instance enabled packet accounting")
			}
		})
	}
}

func TestPrivateHeapPayloadScopeDoesNotShareExchangeState(t *testing.T) {
	t.Setenv("WARP_HOST", "by-us-fmt-5-edge-4")
	t.Setenv("WARP_BLOCK", "g3")
	options := RunOptions{PrivateHeapProfileTarget: "by-us-fmt-5-edge-4/g3"}
	first, second := exchangeSettingsForRun(options), exchangeSettingsForRun(options)
	if first.SDKPayloadOwnerLedger == nil || second.SDKPayloadOwnerLedger == nil || first.SDKPayloadOwnerLedger == second.SDKPayloadOwnerLedger {
		t.Fatal("different exchanges share diagnostic accounting")
	}
	t.Setenv("WARP_BLOCK", "g4")
	if exchangeSettingsForRun(options).SDKPayloadOwnerLedger != nil || first.SDKPayloadOwnerLedger == nil {
		t.Fatal("new instance selection changed an existing captured scope")
	}
}
