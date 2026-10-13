package controller

// A control request whose every frame is a CreateContract this owner already
// refused for the same client, destination and companion flag is answered
// after the credential's signature, claims and session checks and before its
// client state query (router.WrapWithInputRequireClientUnlessAnswered). Old
// provider sdks retry such routes every 5s for hours, and each retry ran the
// state query only to be told again the refusal recorded for that client.
//
// The answer grants nothing: it reports recorded refusals, creates no
// contract and keeps no result of any state query. A removed or rotated
// client keeps receiving the refusals recorded for it until each expires (60s
// in the ring, an hour for a terminal refusal) or an origin edge removes it;
// any other request from it still runs the state query and is refused. A
// refusal is replayed only for the exact route key it was recorded under,
// with the credential's verified client id as the source.
//
// The replay runs the same frame and contract paths as an authenticated
// request, so every per-frame counter reads as for a route rejection hit.
// urnetwork_connect_control_refusal_replays_total counts the requests
// answered this way, which run no state query.

import (
	"context"
	"encoding/base64"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var connectControlRefusalReplayCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_connect_control_refusal_replays_total",
	Help: "Connect control requests answered from route refusals recorded for the credential's own client, after its signature, claims and session checks and without its client state query, by the refusals' cause (mixed when its frames differ).",
}, []string{"cause"})

func init() {
	for _, cause := range []string{"inactive_destination", "missing_companion_origin", "mixed"} {
		connectControlRefusalReplayCounter.WithLabelValues(cause)
	}
	prometheus.MustRegister(connectControlRefusalReplayCounter)
}

// A frame reached the frame or contract path of a replay without a refusal
// prepared for it. Preparation rules this out; nothing is run for it.
var errConnectControlRefusalNotPrepared = errors.New("control frame has no recorded refusal to replay")

type connectControlRefusalsKey struct{}

type connectControlRefusalKey struct {
	destinationId server.Id
	companion     bool
}

// The recorded refusal of every frame of one control pack, for one client.
// Read-only once prepared, so safe for concurrent use.
type ConnectControlRefusals struct {
	clientId           server.Id
	frames             []*protocol.Frame
	routeKeyRejections map[connectControlRefusalKey]model.ContractRouteRejection
	cause              string
}

// Decides from the owner's recorded refusals alone whether every frame of the
// pack is a CreateContract that clientId was already refused for. Performs no
// database, Redis or network work and records nothing.
func PrepareConnectControlRefusals(ctx context.Context, clientId server.Id, connectControl *ConnectControlArgs) (*ConnectControlRefusals, bool) {
	notifications := model.GetContractOriginNotifications(ctx)
	if notifications == nil || connectControl == nil || clientId == ControlId {
		return nil, false
	}
	// a plain decode: the frames must not borrow a pooled buffer
	packBytes, err := base64.StdEncoding.DecodeString(connectControl.Pack)
	if err != nil {
		return nil, false
	}
	pack := &protocol.Pack{}
	if err := proto.Unmarshal(packBytes, pack); err != nil || len(pack.Frames) == 0 {
		return nil, false
	}
	refusals := &ConnectControlRefusals{
		clientId:           clientId,
		frames:             pack.Frames,
		routeKeyRejections: map[connectControlRefusalKey]model.ContractRouteRejection{},
	}
	for _, frame := range pack.Frames {
		message, err := connect.FromFrame(frame)
		if err != nil {
			return nil, false
		}
		createContract, ok := message.(*protocol.CreateContract)
		if !ok {
			return nil, false
		}
		destinationId, err := server.IdFromBytes(createContract.DestinationId)
		if err != nil || destinationId == ControlId {
			return nil, false
		}
		rejection, ok := notifications.RouteRejection(clientId, destinationId, createContract.Companion)
		if !ok {
			return nil, false
		}
		refusals.routeKeyRejections[connectControlRefusalKey{destinationId: destinationId, companion: createContract.Companion}] = rejection
		cause := "inactive_destination"
		if rejection.Cause == model.ContractRouteMissingOrigin {
			cause = "missing_companion_origin"
		}
		switch refusals.cause {
		case "", cause:
			refusals.cause = cause
		default:
			refusals.cause = "mixed"
		}
	}
	return refusals, true
}

func (self *ConnectControlRefusals) rejection(clientId server.Id, destinationId server.Id, companion bool) (model.ContractRouteRejection, bool) {
	if clientId != self.clientId {
		return model.ContractRouteRejection{}, false
	}
	rejection, ok := self.routeKeyRejections[connectControlRefusalKey{destinationId: destinationId, companion: companion}]
	return rejection, ok
}

func connectControlRefusalsFromContext(ctx context.Context) *ConnectControlRefusals {
	refusals, _ := ctx.Value(connectControlRefusalsKey{}).(*ConnectControlRefusals)
	return refusals
}

// Reports the prepared refusals again through the frame and contract paths of
// an authenticated request. A frame or route without a prepared refusal is
// refused there without being run.
func ReplayConnectControlRefusals(ctx context.Context, refusals *ConnectControlRefusals) (*ConnectControlResult, error) {
	ctx = context.WithValue(ctx, controlHttpIngressKey{}, true)
	ctx = context.WithValue(ctx, connectControlRefusalsKey{}, refusals)
	resultFrames, resultErr := ConnectControlFrames(ctx, refusals.clientId, refusals.frames, connect.DefaultContractManagerSettings())
	defer returnConnectControlFrames(resultFrames)
	result, err := connectControlResult(resultFrames, resultErr)
	if err != nil {
		return nil, err
	}
	connectControlRefusalReplayCounter.WithLabelValues(refusals.cause).Inc()
	return result, nil
}
