//go:build sidecar_pending

package model

// approval_use (approval_use_model.go; §16.6's gate, D-MCP-11, AU1-AU4, FS-25's
// gate legs on the model, plan-MCP-fs25-gate-before-dns): with a test effect in
// the gate's own transaction, the first spend commits; a lost response after
// COMMIT replays the stored result, past not_after too; two racers for one key
// make one row; two instances racing on a barrier, one wins use_index 0; uses = 2
// with three actions, then 409 SPENT; a refusal writes nothing; the
// Idempotency-Key must be the action's effect key; a cancel inserts the remaining
// rows, and is refused for another signer; and the sweep removes rows a day past
// their decision's not_after. The spend check's state is a fixture: the decisions
// are real, signed by a device key it admits.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// One device of the fixture: its sidecar, member and key.
type testApprovalDevice struct {
	sidecarId sidecar.Id
	memberId  sidecar.Id
	key       ed25519.PrivateKey
}

func newTestApprovalDevice(label string, memberId sidecar.Id) testApprovalDevice {
	seed := sha256.Sum256([]byte("approval use test " + label))
	idHash := sha256.Sum256([]byte("approval use test id " + label))
	return testApprovalDevice{sidecarId: sidecar.Id(idHash[:16]), memberId: memberId, key: ed25519.NewKeyFromSeed(seed[:])}
}

// A spend state of fixed facts: the devices it admits, each approval-capable for
// its member, and one rule whose approver is approver on channel device.
type testApprovalState struct {
	pin      *bridge.AuthPin
	devices  []testApprovalDevice
	approver sidecar.Id
}

// Its pin.
func (self *testApprovalState) Pin() *bridge.AuthPin {
	return self.pin
}

// The pin's own digest.
func (self *testApprovalState) ChainAt(version uint64) ([sha256.Size]byte, bool) {
	if version == self.pin.GetVersion() {
		return [sha256.Size]byte(self.pin.GetChain()), true
	}
	return [sha256.Size]byte{}, false
}

// Never forked.
func (self *testApprovalState) Forked() bool {
	return false
}

// A device's key.
func (self *testApprovalState) IdentityKey(sidecarId sidecar.Id) []byte {
	for _, device := range self.devices {
		if device.sidecarId == sidecarId {
			return device.key.Public().(ed25519.PublicKey)
		}
	}
	return nil
}

// Each device of its member.
func (self *testApprovalState) ApprovalCapable(sidecarId sidecar.Id, memberId sidecar.Id) bool {
	for _, device := range self.devices {
		if device.sidecarId == sidecarId && device.memberId == memberId {
			return true
		}
	}
	return false
}

// No presence key.
func (self *testApprovalState) PresenceKey(keyId []byte, memberId sidecar.Id) *bridge.KeyRow {
	return nil
}

// The one rule.
func (self *testApprovalState) Govern(action *sidecar.StoredCanonicalAction, spender sidecar.Id, now time.Time) (sidecar.SpendRule, error) {
	return self, nil
}

// Device evidence, known.
func (self *testApprovalState) Evidence() ([]protocol.ApprovalChannel, bool) {
	return []protocol.ApprovalChannel{protocol.ApprovalChannel_APPROVAL_CHANNEL_DEVICE}, true
}

// k = 1.
func (self *testApprovalState) ApproverCount() int {
	return 1
}

// The approver alone.
func (self *testApprovalState) Eligible(memberId sidecar.Id) bool {
	return memberId == self.approver
}

// No admin clause.
func (self *testApprovalState) AdminClauseMet(memberIds []sidecar.Id) bool {
	return true
}

// The fixture: a network, its requester laptop, the approver's phone and another
// member's device, and the state that admits them.
type testApprovalWorld struct {
	networkId server.Id
	laptop    testApprovalDevice
	phone     testApprovalDevice
	stranger  testApprovalDevice
	state     *testApprovalState
}

func newTestApprovalWorld() *testApprovalWorld {
	member := sidecar.NewId()
	other := sidecar.NewId()
	chain := sha256.Sum256([]byte("approval use test chain"))
	world := &testApprovalWorld{
		networkId: server.NewId(),
		laptop:    newTestApprovalDevice("laptop "+member.String(), member),
		phone:     newTestApprovalDevice("phone "+member.String(), member),
		stranger:  newTestApprovalDevice("stranger "+other.String(), other),
	}
	world.state = &testApprovalState{
		pin:      &bridge.AuthPin{Version: 9, Chain: chain[:]},
		devices:  []testApprovalDevice{world.laptop, world.phone, world.stranger},
		approver: member,
	}
	return world
}

// A dns_set action of value, or a template of the name and type with scope set.
func testApprovalDnsAction(t testing.TB, value string, template bool) *sidecar.StoredCanonicalAction {
	t.Helper()
	action := &protocol.CanonicalAction{Kind: uint32(sidecar.SidecarActionDnsSet), DnsName: "www", DnsType: "A", DnsValue: value, DnsTtl: 300}
	if template {
		action = &protocol.CanonicalAction{Kind: uint32(sidecar.SidecarActionDnsSet), Scope: true, DnsName: "www", DnsType: "A"}
	}
	stored, err := sidecar.NewCanonicalAction(action)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// signer's Signed decision for bound, of uses, expiring at notAfter, requested by
// the laptop, as stored.
func (self *testApprovalWorld) decision(t testing.TB, bound *sidecar.StoredCanonicalAction, approvalId sidecar.Id, uses uint32, notAfter time.Time, signer testApprovalDevice) []byte {
	t.Helper()
	payload := bound.PayloadSha256()
	requestSha256 := sha256.Sum256(append([]byte("approval use test request "), approvalId[:]...))
	body := &protocol.ApprovalDecision{
		ApprovalId:         approvalId.Bytes(),
		RequestSha256:      requestSha256[:],
		PayloadSha256:      payload[:],
		Decision:           uint32(protocol.ApprovalVerdict_APPROVAL_VERDICT_APPROVE),
		Uses:               uses,
		NotAfterMillis:     notAfter.UnixMilli(),
		ApproverMemberId:   signer.memberId.Bytes(),
		Channel:            uint32(protocol.ApprovalChannel_APPROVAL_CHANNEL_DEVICE),
		RequesterSidecarId: self.laptop.sidecarId.Bytes(),
		AuthPin:            self.state.pin,
		Version:            1,
	}
	return testApprovalSigned(t, signer, bridge.SignedType_SIGNED_APPROVAL_DECISION, body)
}

// body signed by device's identity key, as stored.
func testApprovalSigned(t testing.TB, device testApprovalDevice, signedType bridge.SignedType, body proto.Message) []byte {
	t.Helper()
	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	signed := &bridge.Signed{
		Body:      bodyBytes,
		SignerId:  device.sidecarId.Bytes(),
		Signature: ed25519.Sign(device.key, bridge.SignedBytes(sidecar.ApprovalPurpose, signedType, bodyBytes)),
		Type:      uint32(signedType),
	}
	encoded, err := proto.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// The request presenting decision for use, under template where set, with use's
// effect key and the fields its own dns write shows.
func (self *testApprovalWorld) request(decision []byte, approvalId sidecar.Id, use *sidecar.StoredCanonicalAction, template *sidecar.StoredCanonicalAction) *ApprovalUseRequest {
	effectKey := sidecar.ApprovalEffectKey(approvalId, use.ActionId(approvalId))
	request := &ApprovalUseRequest{
		NetworkId:        self.networkId,
		SpenderSidecarId: server.Id(self.laptop.sidecarId),
		Decisions:        [][]byte{decision},
		Action:           use.Bytes,
		EffectKey:        effectKey[:],
		Fields: &sidecar.SpendGateFields{
			Kind: sidecar.SidecarActionDnsSet, DnsName: use.Action.GetDnsName(), DnsType: use.Action.GetDnsType(),
			DnsValue: use.Action.GetDnsValue(), DnsTtl: use.Action.GetDnsTtl(),
		},
	}
	if template != nil {
		request.Template = template.Bytes
	}
	return request
}

// A test effect that counts its runs and answers which run it was.
type testApprovalEffect struct {
	count atomic.Int64
}

func (self *testApprovalEffect) apply(ctx context.Context, tx server.PgTx, useIndex int) ([]byte, error) {
	run := self.count.Add(1)
	return []byte(fmt.Sprintf("effect %d of use %d", run, useIndex)), nil
}

// The gate in its own transaction, committed, at the default isolation unless
// options say another.
func (self *testApprovalWorld) consume(ctx context.Context, request *ApprovalUseRequest, now time.Time, apply ApprovalUseEffect, options ...any) (*ApprovalUseOutcome, error) {
	var outcome *ApprovalUseOutcome
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		outcome, err = consumeApprovalUseInTx(ctx, tx, request, self.state, now, apply)
	}, options...)
	return outcome, err
}

// The approval's rows: (use_index, state, whether a result is held).
func testApprovalRows(t testing.TB, ctx context.Context, approvalId sidecar.Id) []string {
	t.Helper()
	rows := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT use_index, state, result IS NOT NULL FROM approval_use WHERE approval_id = $1 ORDER BY use_index`, server.Id(approvalId))
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var useIndex int
				var state string
				var hasResult bool
				server.Raise(result.Scan(&useIndex, &state, &hasResult))
				rows = append(rows, fmt.Sprintf("%d %s %t", useIndex, state, hasResult))
			}
		})
	})
	return rows
}

// The first spend commits use 0 with its result; a retry after its response was
// lost replays that result with no effect, past not_after too.
func TestApprovalUseFirstSpendCommitsAndAReplayAnswersIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestApprovalWorld()
		now := server.NowUtc()
		action := testApprovalDnsAction(t, "192.0.2.30", false)
		approvalId := sidecar.NewId()
		decision := world.decision(t, action, approvalId, 1, now.Add(time.Hour), world.phone)
		effect := &testApprovalEffect{}

		first, err := world.consume(ctx, world.request(decision, approvalId, action, nil), now, effect.apply)
		if err != nil || first.Replayed || first.UseIndex != 0 || string(first.Result) != "effect 1 of use 0" {
			t.Fatalf("the first spend: %+v %v", first, err)
		}
		if rows := testApprovalRows(t, ctx, approvalId); len(rows) != 1 || rows[0] != "0 done true" {
			t.Fatalf("rows %v", rows)
		}
		for _, at := range []time.Time{now.Add(time.Minute), now.Add(2 * time.Hour)} {
			replay, err := world.consume(ctx, world.request(decision, approvalId, action, nil), at, effect.apply)
			if err != nil || !replay.Replayed || string(replay.Result) != string(first.Result) || replay.UseIndex != 0 {
				t.Errorf("a replay at %s: %+v %v", at, replay, err)
			}
		}
		if effect.count.Load() != 1 {
			t.Errorf("%d effects", effect.count.Load())
		}
		// a first presentation past not_after is refused at the gate's own clock, no skew allowed
		late := testApprovalDnsAction(t, "192.0.2.31", false)
		lateId := sidecar.NewId()
		lateDecision := world.decision(t, late, lateId, 1, now.Add(-30*time.Second), world.phone)
		_, err = world.consume(ctx, world.request(lateDecision, lateId, late, nil), now, effect.apply)
		if approvalErr := (&ApprovalUseError{}); !errors.As(err, &approvalErr) || approvalErr.Status != http.StatusForbidden {
			t.Errorf("a late first presentation: %v", err)
		}
	})
}

// A refusal writes nothing: another requester's decision, an effect key that is not
// the action's, and a field the gate's request does not show.
func TestApprovalUseRefusalWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestApprovalWorld()
		now := server.NowUtc()
		action := testApprovalDnsAction(t, "192.0.2.32", false)
		approvalId := sidecar.NewId()
		decision := world.decision(t, action, approvalId, 1, now.Add(time.Hour), world.phone)
		effect := &testApprovalEffect{}

		another := world.request(decision, approvalId, action, nil)
		another.SpenderSidecarId = server.Id(world.stranger.sidecarId)
		wrongKey := world.request(decision, approvalId, action, nil)
		wrongKey.EffectKey = make([]byte, 32)
		otherValue := world.request(decision, approvalId, action, nil)
		otherValue.Fields.DnsValue = "192.0.2.99"
		strangerSigned := world.request(world.decision(t, action, approvalId, 1, now.Add(time.Hour), world.stranger), approvalId, action, nil)
		for name, c := range map[string]struct {
			request *ApprovalUseRequest
			status  int
		}{
			"another requester":              {request: another, status: http.StatusForbidden},
			"another effect key":             {request: wrongKey, status: http.StatusBadRequest},
			"a field the request differs on": {request: otherValue, status: http.StatusForbidden},
			"a non-approver's decision":      {request: strangerSigned, status: http.StatusForbidden},
		} {
			_, err := world.consume(ctx, c.request, now, effect.apply)
			approvalErr := &ApprovalUseError{}
			if !errors.As(err, &approvalErr) || approvalErr.Status != c.status {
				t.Errorf("%s: %v", name, err)
			}
		}
		if rows := testApprovalRows(t, ctx, approvalId); len(rows) != 0 || effect.count.Load() != 0 {
			t.Errorf("a refusal wrote %v and ran %d effects", rows, effect.count.Load())
		}
	})
}

// Two requests in flight at once, ordered by the claim seam: the second has read
// its uses before the first commits, then inserts. For one effect key the second
// answers the first's row, one row and one effect; for two actions under a
// template of uses 2 the second takes use 1; for two actions under uses 1 the
// second finds no use left, 409 SPENT, the first winning use_index 0. Each runs
// under READ COMMITTED, where the gate's own conflict answers, and under the
// default REPEATABLE READ, where the second's transaction reruns and answers.
func TestApprovalUseRacersSerializeOnTheKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestApprovalWorld()
		now := server.NowUtc()
		race := func(first *ApprovalUseRequest, second *ApprovalUseRequest, effect *testApprovalEffect, options ...any) (*ApprovalUseOutcome, *ApprovalUseOutcome, error) {
			inEffect := make(chan struct{})
			releaseFirst := make(chan struct{})
			firstDone := make(chan struct{})
			secondReady := make(chan struct{})
			releaseSecond := make(chan struct{})
			var firstOutcome, secondOutcome *ApprovalUseOutcome
			var firstErr, secondErr error
			var firstOnce, secondOnce sync.Once
			var waitGroup sync.WaitGroup
			waitGroup.Add(2)
			go func() {
				defer waitGroup.Done()
				defer close(firstDone)
				firstOutcome, firstErr = world.consume(ctx, first, now, func(ctx context.Context, tx server.PgTx, useIndex int) ([]byte, error) {
					firstOnce.Do(func() {
						close(inEffect)
						<-releaseFirst
					})
					return effect.apply(ctx, tx, useIndex)
				}, options...)
			}()
			<-inEffect
			go func() {
				defer waitGroup.Done()
				// a rerun of the transaction passes the seam again, and goes on
				hooked := context.WithValue(ctx, approvalUseClaimHookKey{}, func() {
					secondOnce.Do(func() {
						close(secondReady)
						<-releaseSecond
					})
				})
				secondOutcome, secondErr = world.consume(hooked, second, now, effect.apply, options...)
			}()
			<-secondReady
			close(releaseFirst)
			<-firstDone
			close(releaseSecond)
			waitGroup.Wait()
			if firstErr != nil {
				t.Fatalf("the first: %v", firstErr)
			}
			return firstOutcome, secondOutcome, secondErr
		}

		for name, options := range map[string][]any{"read committed": {server.TxReadCommitted}, "repeatable read": nil} {
			action := testApprovalDnsAction(t, "192.0.2.33", false)
			approvalId := sidecar.NewId()
			decision := world.decision(t, action, approvalId, 1, now.Add(time.Hour), world.phone)
			effect := &testApprovalEffect{}
			first, second, err := race(world.request(decision, approvalId, action, nil), world.request(decision, approvalId, action, nil), effect, options...)
			if err != nil || !second.Replayed || string(second.Result) != string(first.Result) || first.UseIndex != 0 {
				t.Errorf("%s, one key: %+v then %+v, %v", name, first, second, err)
			}
			if rows := testApprovalRows(t, ctx, approvalId); len(rows) != 1 || effect.count.Load() != 1 {
				t.Errorf("%s, one key: rows %v, %d effects", name, rows, effect.count.Load())
			}

			template := testApprovalDnsAction(t, "", true)
			pairId := sidecar.NewId()
			pairDecision := world.decision(t, template, pairId, 2, now.Add(time.Hour), world.phone)
			pairEffect := &testApprovalEffect{}
			first, second, err = race(
				world.request(pairDecision, pairId, testApprovalDnsAction(t, "192.0.2.35", false), template),
				world.request(pairDecision, pairId, testApprovalDnsAction(t, "192.0.2.36", false), template),
				pairEffect, options...,
			)
			if err != nil || first.UseIndex != 0 || second.UseIndex != 1 || second.Replayed || pairEffect.count.Load() != 2 {
				t.Errorf("%s, uses 2: %+v then %+v, %v", name, first, second, err)
			}

			singleId := sidecar.NewId()
			singleDecision := world.decision(t, template, singleId, 2, now.Add(time.Hour), world.phone)
			// a template of uses 2 whose first use is spent leaves one, which two instances race for
			if _, err := world.consume(ctx, world.request(singleDecision, singleId, testApprovalDnsAction(t, "192.0.2.37", false), template), now, (&testApprovalEffect{}).apply); err != nil {
				t.Fatal(err)
			}
			lastEffect := &testApprovalEffect{}
			first, _, err = race(
				world.request(singleDecision, singleId, testApprovalDnsAction(t, "192.0.2.38", false), template),
				world.request(singleDecision, singleId, testApprovalDnsAction(t, "192.0.2.39", false), template),
				lastEffect, options...,
			)
			if !errors.Is(err, ErrApprovalUseSpent) || first.UseIndex != 1 || lastEffect.count.Load() != 1 {
				t.Errorf("%s, the last use: %+v, %v, %d effects", name, first, err, lastEffect.count.Load())
			}
		}
	})
}

// uses = 2 with three actions under one template: two effects, then 409 SPENT.
func TestApprovalUseTwoUsesThreeActions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestApprovalWorld()
		now := server.NowUtc()
		template := testApprovalDnsAction(t, "", true)
		approvalId := sidecar.NewId()
		decision := world.decision(t, template, approvalId, 2, now.Add(time.Hour), world.phone)
		effect := &testApprovalEffect{}
		for i, value := range []string{"192.0.2.40", "192.0.2.41"} {
			outcome, err := world.consume(ctx, world.request(decision, approvalId, testApprovalDnsAction(t, value, false), template), now, effect.apply)
			if err != nil || outcome.UseIndex != i || outcome.Replayed {
				t.Fatalf("use %d: %+v %v", i, outcome, err)
			}
		}
		_, err := world.consume(ctx, world.request(decision, approvalId, testApprovalDnsAction(t, "192.0.2.42", false), template), now, effect.apply)
		approvalErr := &ApprovalUseError{}
		if !errors.As(err, &approvalErr) || approvalErr.Status != http.StatusConflict || !errors.Is(err, ErrApprovalUseSpent) {
			t.Errorf("a third action: %v", err)
		}
		if rows := testApprovalRows(t, ctx, approvalId); len(rows) != 2 || effect.count.Load() != 2 {
			t.Errorf("rows %v, %d effects", rows, effect.count.Load())
		}
		// an action outside the template is refused before any use
		_, err = world.consume(ctx, world.request(decision, approvalId, testApprovalDnsActionNamed(t, "mail"), template), now, effect.apply)
		if !errors.As(err, &approvalErr) || approvalErr.Status != http.StatusBadRequest {
			t.Errorf("outside the template: %v", err)
		}
	})
}

// A dns_set of another name.
func testApprovalDnsActionNamed(t testing.TB, name string) *sidecar.StoredCanonicalAction {
	t.Helper()
	stored, err := sidecar.NewCanonicalAction(&protocol.CanonicalAction{Kind: uint32(sidecar.SidecarActionDnsSet), DnsName: name, DnsType: "A", DnsValue: "192.0.2.43"})
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// The approver's cancel after one of three uses inserts the two left as cancelled,
// and no gate spends them; a cancel another member's device signed inserts nothing.
func TestCancelInsertsRemainingRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestApprovalWorld()
		now := server.NowUtc()
		template := testApprovalDnsAction(t, "", true)
		approvalId := sidecar.NewId()
		decision := world.decision(t, template, approvalId, 3, now.Add(time.Hour), world.phone)
		effect := &testApprovalEffect{}
		if _, err := world.consume(ctx, world.request(decision, approvalId, testApprovalDnsAction(t, "192.0.2.50", false), template), now, effect.apply); err != nil {
			t.Fatal(err)
		}
		cancel := testApprovalSigned(t, world.phone, bridge.SignedType_SIGNED_APPROVAL_CANCEL, &protocol.ApprovalCancel{ApprovalId: approvalId.Bytes()})
		count, err := cancelApprovalUses(ctx, world.networkId, cancel, decision, world.state, now, nil)
		if err != nil || count != 2 {
			t.Fatalf("the cancel: %d %v", count, err)
		}
		if rows := testApprovalRows(t, ctx, approvalId); len(rows) != 3 || rows[0] != "0 done true" || rows[1] != "1 cancelled false" || rows[2] != "2 cancelled false" {
			t.Errorf("rows %v", rows)
		}
		_, err = world.consume(ctx, world.request(decision, approvalId, testApprovalDnsAction(t, "192.0.2.51", false), template), now, effect.apply)
		if !errors.Is(err, ErrApprovalUseSpent) {
			t.Errorf("a use after the cancel: %v", err)
		}
		if again, err := cancelApprovalUses(ctx, world.networkId, cancel, decision, world.state, now, nil); err != nil || again != 0 {
			t.Errorf("a second cancel: %d %v", again, err)
		}
	})
}

// A cancel another member's device signed, or one naming another approval, is
// refused and inserts nothing.
func TestCancelRefusedForOtherSigner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestApprovalWorld()
		now := server.NowUtc()
		action := testApprovalDnsAction(t, "192.0.2.52", false)
		approvalId := sidecar.NewId()
		decision := world.decision(t, action, approvalId, 1, now.Add(time.Hour), world.phone)
		for name, cancel := range map[string][]byte{
			"another member's device": testApprovalSigned(t, world.stranger, bridge.SignedType_SIGNED_APPROVAL_CANCEL, &protocol.ApprovalCancel{ApprovalId: approvalId.Bytes()}),
			"another approval":        testApprovalSigned(t, world.phone, bridge.SignedType_SIGNED_APPROVAL_CANCEL, &protocol.ApprovalCancel{ApprovalId: sidecar.NewId().Bytes()}),
		} {
			_, err := cancelApprovalUses(ctx, world.networkId, cancel, decision, world.state, now, nil)
			approvalErr := &ApprovalUseError{}
			if !errors.As(err, &approvalErr) || approvalErr.Status != http.StatusForbidden {
				t.Errorf("%s: %v", name, err)
			}
		}
		if rows := testApprovalRows(t, ctx, approvalId); len(rows) != 0 {
			t.Errorf("a refused cancel wrote %v", rows)
		}
	})
}

// The sweep removes rows a day past their decision's not_after and keeps the rest;
// with no gate states installed, the gate and the cancel answer unavailable.
func TestApprovalUseSweepAndUnavailable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestApprovalWorld()
		now := server.NowUtc()
		effect := &testApprovalEffect{}
		oldAction := testApprovalDnsAction(t, "192.0.2.60", false)
		oldId := sidecar.NewId()
		if _, err := world.consume(ctx, world.request(world.decision(t, oldAction, oldId, 1, now.Add(time.Minute), world.phone), oldId, oldAction, nil), now, effect.apply); err != nil {
			t.Fatal(err)
		}
		newAction := testApprovalDnsAction(t, "192.0.2.61", false)
		newId := sidecar.NewId()
		if _, err := world.consume(ctx, world.request(world.decision(t, newAction, newId, 1, now.Add(3*24*time.Hour), world.phone), newId, newAction, nil), now, effect.apply); err != nil {
			t.Fatal(err)
		}
		removed := RemoveExpiredApprovalUses(ctx, now.Add(2*24*time.Hour).Add(-DefaultApprovalUseSettings().Retention))
		if removed != 1 || len(testApprovalRows(t, ctx, oldId)) != 0 || len(testApprovalRows(t, ctx, newId)) != 1 {
			t.Errorf("the sweep removed %d", removed)
		}

		SetApprovalGateStates(nil)
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			_, err = ConsumeApprovalUseInTx(ctx, tx, world.request(world.decision(t, newAction, newId, 1, now.Add(time.Hour), world.phone), newId, newAction, nil), now, effect.apply)
		})
		approvalErr := &ApprovalUseError{}
		if !errors.As(err, &approvalErr) || approvalErr.Status != http.StatusServiceUnavailable {
			t.Errorf("the gate with no states: %v", err)
		}
		if _, err := CancelApprovalUses(ctx, world.networkId, []byte{1}, []byte{1}, now, nil); !errors.As(err, &approvalErr) || approvalErr.Status != http.StatusServiceUnavailable {
			t.Errorf("the cancel with no states: %v", err)
		}
		SetApprovalGateStates(func(ctx context.Context, networkId server.Id) (sidecar.SpendState, error) {
			return world.state, nil
		})
		defer SetApprovalGateStates(nil)
		server.Tx(ctx, func(tx server.PgTx) {
			_, err = ConsumeApprovalUseInTx(ctx, tx, world.request(world.decision(t, newAction, newId, 1, now.Add(time.Hour), world.phone), newId, newAction, nil), now, effect.apply)
		})
		if err != nil {
			t.Errorf("the gate with states installed, a replay: %v", err)
		}
	})
}
