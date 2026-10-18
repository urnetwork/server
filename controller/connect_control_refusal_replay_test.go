package controller

// Preparation and replay of a control request answered before its client
// state query, from refusals recorded in a real origin notification owner.
// Neither reads PostgreSQL or Redis, so these tests run without a database:
// a replay that reached a read would fail here instead of answering.

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func newRefusalReplayOwnerContext(t *testing.T) context.Context {
	t.Helper()
	owner := model.NewContractOriginNotifications(t.Context(), model.DefaultContractOriginNotificationSettings())
	t.Cleanup(owner.Close)
	return model.WithContractOriginNotifications(t.Context(), owner)
}

func refusalReplayInactive() model.ContractRouteRejection {
	return model.ContractRouteRejection{
		Cause:          model.ContractRouteDestinationInactive,
		ResolutionPath: contractResolutionRejected,
		Relationship: model.ProvideRelationshipDetails{
			Mode:                 model.ProvideModePublic,
			SourceLifecycle:      model.NetworkClientLifecycleActiveTop,
			DestinationLifecycle: model.NetworkClientLifecycleInactiveDerived,
			SourceOwner:          model.NetworkClientSourceOwnerOther,
			DestinationOwner:     model.NetworkClientSourceOwnerEgressProber,
		},
	}
}

func refusalReplayMissingOrigin() model.ContractRouteRejection {
	return model.ContractRouteRejection{
		Cause:          model.ContractRouteMissingOrigin,
		ResolutionPath: contractResolutionStreamFallback,
		Relationship: model.ProvideRelationshipDetails{
			Mode:                 model.ProvideModePublic,
			SourceLifecycle:      model.NetworkClientLifecycleActiveTop,
			DestinationLifecycle: model.NetworkClientLifecycleActiveDerived,
			SourceOwner:          model.NetworkClientSourceOwnerOther,
			DestinationOwner:     model.NetworkClientSourceOwnerOther,
		},
	}
}

// The control request input for frames, which it takes.
func refusalReplayArgs(t *testing.T, messages ...proto.Message) *ConnectControlArgs {
	t.Helper()
	var frames []*protocol.Frame
	for _, message := range messages {
		frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	packBytes, err := proto.Marshal(&protocol.Pack{Frames: frames})
	for _, frame := range frames {
		connect.MessagePoolReturn(frame.MessageBytes)
	}
	if err != nil {
		t.Fatal(err)
	}
	return &ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(packBytes)}
}

func refusalReplayCreate(destinationId server.Id, companion bool) *protocol.CreateContract {
	return &protocol.CreateContract{DestinationId: destinationId.Bytes(), TransferByteCount: uint64(1024 * 1024), Companion: companion}
}

func refusalReplayCounter(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			matched := 0
			for _, label := range metric.GetLabel() {
				if value, ok := labels[label.GetName()]; ok && value == label.GetValue() {
					matched += 1
				}
			}
			if matched == len(labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// Only a pack whose every frame is a CreateContract with a refusal recorded
// for this exact client and route key is prepared; the cause is the frames'
// cause, or mixed.
func TestPrepareConnectControlRefusalsOnlyForRecordedRoutes(t *testing.T) {
	ctx := newRefusalReplayOwnerContext(t)
	owner := model.GetContractOriginNotifications(ctx)
	clientId, otherClientId := server.NewId(), server.NewId()
	retiredId, windowId, unrecordedId := server.NewId(), server.NewId(), server.NewId()
	owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
	owner.RecordRouteRejection(clientId, windowId, false, refusalReplayMissingOrigin(), nil)

	for _, prepared := range []struct {
		name  string
		args  *ConnectControlArgs
		cause string
	}{
		{name: "one inactive destination", args: refusalReplayArgs(t, refusalReplayCreate(retiredId, false)), cause: "inactive_destination"},
		{name: "one missing origin", args: refusalReplayArgs(t, refusalReplayCreate(windowId, false)), cause: "missing_companion_origin"},
		{name: "both", args: refusalReplayArgs(t, refusalReplayCreate(retiredId, false), refusalReplayCreate(windowId, false)), cause: "mixed"},
	} {
		refusals, ok := PrepareConnectControlRefusals(ctx, clientId, prepared.args)
		if !ok || refusals.cause != prepared.cause {
			t.Fatalf("%s: prepared=%t cause=%v, want %s", prepared.name, ok, refusals, prepared.cause)
		}
	}

	closeContract := &protocol.CloseContract{ContractId: server.NewId().Bytes()}
	for _, unprepared := range []struct {
		name     string
		ctx      context.Context
		clientId server.Id
		args     *ConnectControlArgs
	}{
		{name: "another client", ctx: ctx, clientId: otherClientId, args: refusalReplayArgs(t, refusalReplayCreate(retiredId, false))},
		{name: "another companion flag", ctx: ctx, clientId: clientId, args: refusalReplayArgs(t, refusalReplayCreate(retiredId, true))},
		{name: "a route without a refusal", ctx: ctx, clientId: clientId, args: refusalReplayArgs(t, refusalReplayCreate(unrecordedId, false))},
		{name: "a recorded route with an unrecorded one", ctx: ctx, clientId: clientId, args: refusalReplayArgs(t, refusalReplayCreate(retiredId, false), refusalReplayCreate(unrecordedId, false))},
		{name: "a recorded route with another message", ctx: ctx, clientId: clientId, args: refusalReplayArgs(t, refusalReplayCreate(retiredId, false), closeContract)},
		{name: "the control destination", ctx: ctx, clientId: clientId, args: refusalReplayArgs(t, refusalReplayCreate(ControlId, false))},
		{name: "an empty pack", ctx: ctx, clientId: clientId, args: refusalReplayArgs(t)},
		{name: "a malformed pack", ctx: ctx, clientId: clientId, args: &ConnectControlArgs{Pack: "not a pack"}},
		{name: "no owner", ctx: t.Context(), clientId: clientId, args: refusalReplayArgs(t, refusalReplayCreate(retiredId, false))},
	} {
		if _, ok := PrepareConnectControlRefusals(unprepared.ctx, unprepared.clientId, unprepared.args); ok {
			t.Fatalf("%s was prepared as recorded refusals", unprepared.name)
		}
	}
}

// The replay reports each prepared refusal through the frame and contract
// paths of an authenticated request: one Reliability result per frame,
// counted as route rejection cache hits, and one answered request.
func TestReplayConnectControlRefusalsReportsOnlyRecordedRefusals(t *testing.T) {
	ctx := newRefusalReplayOwnerContext(t)
	owner := model.GetContractOriginNotifications(ctx)
	clientId, retiredId, windowId := server.NewId(), server.NewId(), server.NewId()
	owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
	owner.RecordRouteRejection(clientId, windowId, false, refusalReplayMissingOrigin(), nil)
	refusals, ok := PrepareConnectControlRefusals(ctx, clientId, refusalReplayArgs(t, refusalReplayCreate(retiredId, false), refusalReplayCreate(windowId, false)))
	if !ok {
		t.Fatal("recorded refusals were not prepared")
	}

	hitLabels := map[string]string{"cause": "inactive_destination", "result": "hit"}
	originHitLabels := map[string]string{"cause": "missing_companion_origin", "result": "hit"}
	hitsBefore := refusalReplayCounter(t, "urnetwork_connect_contract_route_rejection_cache_total", hitLabels)
	originHitsBefore := refusalReplayCounter(t, "urnetwork_connect_contract_route_rejection_cache_total", originHitLabels)
	replaysBefore := refusalReplayCounter(t, "urnetwork_connect_control_refusal_replays_total", map[string]string{"cause": "mixed"})
	result, err := ReplayConnectControlRefusals(ctx, refusals)
	if err != nil || result.Error != nil {
		t.Fatalf("replay failed: err=%v result=%+v", err, result)
	}
	resultBytes, err := base64.StdEncoding.DecodeString(result.Pack)
	if err != nil {
		t.Fatal(err)
	}
	var resultPack protocol.Pack
	if err := proto.Unmarshal(resultBytes, &resultPack); err != nil || len(resultPack.Frames) != 2 {
		t.Fatalf("replay returned %d results, want 2: %v", len(resultPack.Frames), err)
	}
	for _, frame := range resultPack.Frames {
		message, err := connect.FromFrame(frame)
		contractResult, ok := message.(*protocol.CreateContractResult)
		if err != nil || !ok || contractResult.Contract != nil || contractResult.Error == nil || *contractResult.Error != protocol.ContractError_Reliability {
			t.Fatalf("a replayed frame was not a Reliability refusal: %+v %v", message, err)
		}
	}
	if hits := refusalReplayCounter(t, "urnetwork_connect_contract_route_rejection_cache_total", hitLabels); hits != hitsBefore+1 {
		t.Fatalf("inactive destination hits rose by %v, want 1", hits-hitsBefore)
	}
	if hits := refusalReplayCounter(t, "urnetwork_connect_contract_route_rejection_cache_total", originHitLabels); hits != originHitsBefore+1 {
		t.Fatalf("missing origin hits rose by %v, want 1", hits-originHitsBefore)
	}
	if replays := refusalReplayCounter(t, "urnetwork_connect_control_refusal_replays_total", map[string]string{"cause": "mixed"}); replays != replaysBefore+1 {
		t.Fatalf("answered requests rose by %v, want 1", replays-replaysBefore)
	}
}

// A replay never runs a frame it did not prepare: another message, or a
// CreateContract for a route without a prepared refusal, is refused before
// any handler or read.
func TestReplayConnectControlRefusalsRunsNothingUnprepared(t *testing.T) {
	ctx := newRefusalReplayOwnerContext(t)
	owner := model.GetContractOriginNotifications(ctx)
	clientId, retiredId := server.NewId(), server.NewId()
	owner.RecordRouteRejection(clientId, retiredId, false, refusalReplayInactive(), nil)
	refusals, ok := PrepareConnectControlRefusals(ctx, clientId, refusalReplayArgs(t, refusalReplayCreate(retiredId, false)))
	if !ok {
		t.Fatal("the recorded refusal was not prepared")
	}
	var frames []*protocol.Frame
	for _, message := range []proto.Message{
		refusalReplayCreate(server.NewId(), false),
		&protocol.CloseContract{ContractId: server.NewId().Bytes()},
		&protocol.Provide{},
	} {
		frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	defer func() {
		for _, frame := range frames {
			connect.MessagePoolReturn(frame.MessageBytes)
		}
	}()
	replayCtx := context.WithValue(ctx, connectControlRefusalsKey{}, refusals)
	resultFrames, err := ConnectControlFrames(replayCtx, clientId, frames, connect.DefaultContractManagerSettings())
	defer returnConnectControlFrames(resultFrames)
	if len(resultFrames) != 0 {
		t.Fatalf("unprepared frames produced %d results", len(resultFrames))
	}
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) != len(frames) {
		t.Fatalf("unprepared frames were not each refused: %v", err)
	}
	for _, frameErr := range joined.Unwrap() {
		if !errors.Is(frameErr, errConnectControlRefusalNotPrepared) {
			t.Fatalf("an unprepared frame failed for another reason: %v", frameErr)
		}
	}
}
