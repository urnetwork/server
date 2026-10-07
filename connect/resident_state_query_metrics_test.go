// Resident control is already authenticated; internal frames are not JWT work.
package connect

import (
	"testing"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
)

// The actual resident handler shares the old internal ingress bucket with
// direct prober control, but emits no JWT query for these verified frames.
// This is a control-boundary test, not a claim that durable frames avoid PG.
func TestConnectStateQueryMetricResidentControlIsNotAuthentication(t *testing.T) {
	ctx := jwt.WithStateQuerySource(t.Context(), jwt.StateQueryProberControl)
	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()
	owner := newResidentController(ctx, server.NewId(), nil, DefaultExchangeSettings())
	defer owner.Close()
	frame, err := clientconnect.ToFrame(&protocol.ControlPing{}, clientconnect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer clientconnect.MessagePoolReturn(frame.MessageBytes)
	frames := make([]*protocol.Frame, 16)
	for i := range frames {
		frames[i] = frame
	}
	internalLabels := map[string]string{"ingress": "internal", "message": "control_ping", "outcome": "handler_ok"}
	httpLabels := map[string]string{"ingress": "http", "message": "control_ping", "outcome": "handler_ok"}
	internalBefore := connectObservedCounter(t, "urnetwork_connect_control_frames_total", internalLabels)
	httpBefore := connectObservedCounter(t, "urnetwork_connect_control_frames_total", httpLabels)
	queriesBefore := connectObservedCounter(t, "urnetwork_jwt_state_queries_total", nil)
	const requests = 32
	for range requests {
		if err := owner.HandleControlFrames(frames); err != nil {
			t.Fatal(err)
		}
	}
	if connectObservedCounter(t, "urnetwork_connect_control_frames_total", internalLabels) != internalBefore+float64(requests*len(frames)) ||
		connectObservedCounter(t, "urnetwork_connect_control_frames_total", httpLabels) != httpBefore {
		t.Fatal("resident control must retain internal ingress")
	}
	if connectObservedCounter(t, "urnetwork_jwt_state_queries_total", nil) != queriesBefore || attempts() != 0 {
		t.Fatal("verified resident ping frames acquired PostgreSQL or counted JWT work")
	}
}
