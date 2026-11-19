package providertunnel

import (
	"context"
	"errors"
	"github.com/urnetwork/connect/v2026"
	"testing"
)

type privateOpenLocalControl struct{}

func (*privateOpenLocalControl) ConnectControl(context.Context, string, *connect.ConnectControlArgs) (*connect.ConnectControlResult, error) {
	return nil, errors.New("fixture stops before control execution")
}

// The actual Open-to-generator handoff preserves explicit local ownership and
// keeps standalone callers on HTTP. The existing helper terminally joins every
// real generator after its deliberate pre-TUN error.
func TestPrivateOpenLocalControlScope(t *testing.T) {
	local := &privateOpenLocalControl{}
	configs := []Config{probeTransportBudgetTestConfig(), probeTransportBudgetTestConfig()}
	configs[0].ClientControl = local
	captured := captureProbeTransportSettings(t, configs)
	if captured[0].ClientControl != local || captured[1].ClientControl != nil {
		t.Fatal("Open broadened or lost local control scope")
	}
}
