//go:build sidecar_pending

package api

// FS-25's gate legs and §22.7's uses row on real Postgres (MCP-M3-24; §16.6's gate, D-MCP-11, D-MCP-12, AU1-AU4,
// FI-7; plan-MCP-fs25-gate-before-dns): at M3 they run through model.ConsumeApprovalUseInTx, the gate a server-gated
// effect's own transaction runs, with the gate states installed as the api process installs them
// (model.SetApprovalGateStates) and a test effect in the same transaction, which pre-authorizes nothing; and the
// cancel runs through the real POST /approval/cancel route, a strict_exp bearer on §7.4's allowlist. At M5-out the
// same legs run again through POST /dns/records (MCP-M5-01). Each refusal has a twin that must succeed.
//
//   - a gate's response lost after COMMIT: the retry is answered from the use's row, one row and one effect, past
//     not_after too, while a first presentation past not_after is refused;
//   - two api instances racing: two transactions in flight at once, the second's insert after the first's, one row
//     per effect key, one winning use_index 0, the other's action spent at uses = 1 or taking use 1 at uses = 2;
//   - uses = 2 with three actions: two effects, then 409 SPENT;
//   - a cancel after one of three uses, posted to POST /approval/cancel with the allowlist on, refuses the rest, and
//     a cancel another member's device signed is refused through the route and inserts nothing.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One device of the schedule's network: its sidecar, its member and its identity key.
type approvalScheduleDevice struct {
	sidecarId sidecar.Id
	memberId  sidecar.Id
	key       ed25519.PrivateKey
}

func newApprovalScheduleDevice(label string, memberId sidecar.Id) approvalScheduleDevice {
	seed := sha256.Sum256([]byte("approval schedule " + label))
	idHash := sha256.Sum256([]byte("approval schedule id " + label))
	return approvalScheduleDevice{sidecarId: sidecar.Id(idHash[:16]), memberId: memberId, key: ed25519.NewKeyFromSeed(seed[:])}
}

// The gate's verified state for the schedule's network: a pin, the devices it admits, each approval-capable for its
// member, and one rule whose approver is the member, on channel device. It is its own rule.
type approvalScheduleState struct {
	pin      *bridge.AuthPin
	devices  []approvalScheduleDevice
	approver sidecar.Id
}

func (self *approvalScheduleState) Pin() *bridge.AuthPin {
	return self.pin
}

func (self *approvalScheduleState) ChainAt(version uint64) ([sha256.Size]byte, bool) {
	if version == self.pin.GetVersion() {
		return [sha256.Size]byte(self.pin.GetChain()), true
	}
	return [sha256.Size]byte{}, false
}

func (self *approvalScheduleState) Forked() bool {
	return false
}

func (self *approvalScheduleState) IdentityKey(sidecarId sidecar.Id) []byte {
	for _, device := range self.devices {
		if device.sidecarId == sidecarId {
			return device.key.Public().(ed25519.PublicKey)
		}
	}
	return nil
}

func (self *approvalScheduleState) ApprovalCapable(sidecarId sidecar.Id, memberId sidecar.Id) bool {
	for _, device := range self.devices {
		if device.sidecarId == sidecarId && device.memberId == memberId {
			return true
		}
	}
	return false
}

func (self *approvalScheduleState) PresenceKey(keyId []byte, memberId sidecar.Id) *bridge.KeyRow {
	return nil
}

func (self *approvalScheduleState) Govern(action *sidecar.StoredCanonicalAction, spender sidecar.Id, now time.Time) (sidecar.SpendRule, error) {
	return self, nil
}

func (self *approvalScheduleState) Evidence() []protocol.ApprovalChannel {
	return []protocol.ApprovalChannel{protocol.ApprovalChannel_APPROVAL_CHANNEL_DEVICE}
}

func (self *approvalScheduleState) ApproverCount() int {
	return 1
}

func (self *approvalScheduleState) Eligible(memberId sidecar.Id) bool {
	return memberId == self.approver
}

func (self *approvalScheduleState) AdminClauseMet(memberIds []sidecar.Id) bool {
	return true
}

// The schedule's world: a network, the member's laptop that requests and spends, its phone that approves, another
// member's device, the state the gate holds, and the effect each use runs.
type approvalScheduleWorld struct {
	networkId server.Id
	laptop    approvalScheduleDevice
	phone     approvalScheduleDevice
	stranger  approvalScheduleDevice
	state     *approvalScheduleState
	effects   atomic.Int64
}

// A world on networkId, its state installed as the api process installs the gate cache's (model.ApprovalGateStatesOf),
// answering the removal.
func newApprovalScheduleWorld(networkId server.Id) (*approvalScheduleWorld, func()) {
	member := sidecar.NewId()
	other := sidecar.NewId()
	chain := sha256.Sum256([]byte("approval schedule chain " + networkId.String()))
	world := &approvalScheduleWorld{
		networkId: networkId,
		laptop:    newApprovalScheduleDevice("laptop "+member.String(), member),
		phone:     newApprovalScheduleDevice("phone "+member.String(), member),
		stranger:  newApprovalScheduleDevice("stranger "+other.String(), other),
	}
	world.state = &approvalScheduleState{
		pin:      &bridge.AuthPin{Version: 4, Chain: chain[:]},
		devices:  []approvalScheduleDevice{world.laptop, world.phone, world.stranger},
		approver: member,
	}
	model.SetApprovalGateStates(func(ctx context.Context, requested server.Id) (sidecar.SpendState, error) {
		if requested != networkId {
			return nil, errors.New("not the schedule's network")
		}
		return world.state, nil
	})
	return world, func() { model.SetApprovalGateStates(nil) }
}

// A dns_set of value, or with template set the template of the name and type, scope set.
func approvalScheduleAction(t testing.TB, value string, template bool) *sidecar.StoredCanonicalAction {
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

// body signed by device's identity key under purpose approval, as stored.
func approvalScheduleSigned(t testing.TB, device approvalScheduleDevice, signedType bridge.SignedType, body proto.Message) []byte {
	t.Helper()
	bodyBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(&bridge.Signed{
		Body:      bodyBytes,
		SignerId:  device.sidecarId.Bytes(),
		Signature: ed25519.Sign(device.key, bridge.SignedBytes(sidecar.ApprovalPurpose, signedType, bodyBytes)),
		Type:      uint32(signedType),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// The phone's decision approving bound for uses, expiring at notAfter, the laptop its requester, as stored.
func (self *approvalScheduleWorld) decision(t testing.TB, bound *sidecar.StoredCanonicalAction, approvalId sidecar.Id, uses uint32, notAfter time.Time) []byte {
	t.Helper()
	payload := bound.PayloadSha256()
	requestSha256 := sha256.Sum256(append([]byte("approval schedule request "), approvalId[:]...))
	return approvalScheduleSigned(t, self.phone, bridge.SignedType_SIGNED_APPROVAL_DECISION, &protocol.ApprovalDecision{
		ApprovalId:         approvalId.Bytes(),
		RequestSha256:      requestSha256[:],
		PayloadSha256:      payload[:],
		Decision:           uint32(protocol.ApprovalVerdict_APPROVAL_VERDICT_APPROVE),
		Uses:               uses,
		NotAfterMillis:     notAfter.UnixMilli(),
		ApproverMemberId:   self.phone.memberId.Bytes(),
		Channel:            uint32(protocol.ApprovalChannel_APPROVAL_CHANNEL_DEVICE),
		RequesterSidecarId: self.laptop.sidecarId.Bytes(),
		AuthPin:            self.state.pin,
		Version:            1,
	})
}

// The gate request the laptop presents for use, under template where set, with use's effect key as its
// Idempotency-Key and the fields the dns write itself shows.
func (self *approvalScheduleWorld) request(decision []byte, approvalId sidecar.Id, use *sidecar.StoredCanonicalAction, template *sidecar.StoredCanonicalAction) *model.ApprovalUseRequest {
	effectKey := sidecar.ApprovalEffectKey(approvalId, use.ActionId(approvalId))
	request := &model.ApprovalUseRequest{
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

// The test effect, in the gate's transaction: counts its runs and answers which it was.
func (self *approvalScheduleWorld) apply(ctx context.Context, tx server.PgTx, useIndex int) ([]byte, error) {
	run := self.effects.Add(1)
	return []byte(fmt.Sprintf("effect %d of use %d", run, useIndex)), nil
}

// The gate in its own transaction at server time now, committed.
func (self *approvalScheduleWorld) consume(ctx context.Context, request *model.ApprovalUseRequest, now time.Time, apply model.ApprovalUseEffect, options ...any) (*model.ApprovalUseOutcome, error) {
	var outcome *model.ApprovalUseOutcome
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		outcome, err = model.ConsumeApprovalUseInTx(ctx, tx, request, now, apply)
	}, options...)
	return outcome, err
}

// The approval's rows: use_index, state, and whether a result is held.
func approvalScheduleRows(t testing.TB, ctx context.Context, approvalId sidecar.Id) []string {
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

// The gate's status of err, 0 for none.
func approvalScheduleStatus(err error) int {
	approvalErr := &model.ApprovalUseError{}
	if errors.As(err, &approvalErr) {
		return approvalErr.Status
	}
	return 0
}

// FS-25's lost response: the first spend commits use 0 and its result; the retry, its response lost after COMMIT,
// replays that row with no second effect, past not_after too. The refusal beside it, a first presentation past
// not_after, writes nothing; its twin is the first spend.
func TestFs25GateResponseLostAfterCommit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, remove := newApprovalScheduleWorld(server.NewId())
		defer remove()
		now := server.NowUtc()
		action := approvalScheduleAction(t, "192.0.2.70", false)
		approvalId := sidecar.NewId()
		decision := world.decision(t, action, approvalId, 1, now.Add(time.Hour))
		first, err := world.consume(ctx, world.request(decision, approvalId, action, nil), now, world.apply)
		if err != nil || first.Replayed || first.UseIndex != 0 {
			t.Fatalf("the first spend: %+v %v", first, err)
		}
		// the response is lost: the same request again, now and past the decision's not_after
		for _, at := range []time.Time{now.Add(time.Second), now.Add(2 * time.Hour)} {
			replay, err := world.consume(ctx, world.request(decision, approvalId, action, nil), at, world.apply)
			if err != nil || !replay.Replayed || string(replay.Result) != string(first.Result) {
				t.Errorf("a retry at %s: %+v %v", at, replay, err)
			}
		}
		if rows := approvalScheduleRows(t, ctx, approvalId); len(rows) != 1 || rows[0] != "0 done true" || world.effects.Load() != 1 {
			t.Errorf("rows %v, %d effects", rows, world.effects.Load())
		}
		late := approvalScheduleAction(t, "192.0.2.71", false)
		lateId := sidecar.NewId()
		_, err = world.consume(ctx, world.request(world.decision(t, late, lateId, 1, now.Add(-time.Second)), lateId, late, nil), now, world.apply)
		if approvalScheduleStatus(err) != http.StatusForbidden || len(approvalScheduleRows(t, ctx, lateId)) != 0 {
			t.Errorf("a first presentation past not_after: %v", err)
		}
	})
}

// FS-25's two api instances: the first's transaction holds its use uncommitted while the second, another instance's,
// runs the gate, and the first then commits. One effect key, on an exact action's decision of uses = 1: the second
// answers the first's row, one row and one effect, the first winning use_index 0. The last use of a uses = 2 template,
// its use 0 spent before: the first's action wins use 1 and the second's is 409 SPENT. Its twin, a uses = 3 template:
// the second's action takes use 2.
func TestFs25TwoApiInstancesRacing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, remove := newApprovalScheduleWorld(server.NewId())
		defer remove()
		now := server.NowUtc()
		// the two instances' transactions: the first held after its insert, the second run to its answer, the first
		// then released to commit; every interleaving of the second's statements with the first's commit answers alike
		race := func(name string, first *model.ApprovalUseRequest, second *model.ApprovalUseRequest) (*model.ApprovalUseOutcome, *model.ApprovalUseOutcome, error) {
			holding := make(chan struct{})
			release := make(chan struct{})
			firstDone := make(chan error, 1)
			var firstOutcome *model.ApprovalUseOutcome
			go func() {
				var err error
				firstOutcome, err = world.consume(ctx, first, now, func(ctx context.Context, tx server.PgTx, useIndex int) ([]byte, error) {
					close(holding)
					<-release
					return world.apply(ctx, tx, useIndex)
				})
				firstDone <- err
			}()
			select {
			case <-holding:
			case err := <-firstDone:
				t.Fatalf("%s: the first instance never held its use: %v", name, err)
			}
			secondDone := make(chan struct{})
			var secondOutcome *model.ApprovalUseOutcome
			var secondErr error
			go func() {
				defer close(secondDone)
				secondOutcome, secondErr = world.consume(ctx, second, now, world.apply, server.TxReadCommitted)
			}()
			close(release)
			if err := <-firstDone; err != nil {
				t.Fatalf("%s: the first instance: %v", name, err)
			}
			<-secondDone
			return firstOutcome, secondOutcome, secondErr
		}

		action := approvalScheduleAction(t, "192.0.2.80", false)
		approvalId := sidecar.NewId()
		decision := world.decision(t, action, approvalId, 1, now.Add(time.Hour))
		first, second, err := race("one effect key", world.request(decision, approvalId, action, nil), world.request(decision, approvalId, action, nil))
		if err != nil || first.UseIndex != 0 || second.UseIndex != 0 || !second.Replayed || string(second.Result) != string(first.Result) {
			t.Errorf("one effect key: %+v %+v %v", first, second, err)
		}
		if rows := approvalScheduleRows(t, ctx, approvalId); len(rows) != 1 {
			t.Errorf("one effect key: rows %v", rows)
		}

		// a template of uses, its use 0 spent before the race
		lastUse := func(uses uint32) (*model.ApprovalUseOutcome, *model.ApprovalUseOutcome, error, sidecar.Id) {
			template := approvalScheduleAction(t, "", true)
			approvalId := sidecar.NewId()
			decision := world.decision(t, template, approvalId, uses, now.Add(time.Hour))
			for index := range 1 {
				spent := approvalScheduleAction(t, fmt.Sprintf("192.0.2.%d", 90+index), false)
				if _, err := world.consume(ctx, world.request(decision, approvalId, spent, template), now, world.apply); err != nil {
					t.Fatalf("a use before the race: %v", err)
				}
			}
			first, second, err := race(fmt.Sprintf("uses %d", uses),
				world.request(decision, approvalId, approvalScheduleAction(t, "192.0.2.81", false), template),
				world.request(decision, approvalId, approvalScheduleAction(t, "192.0.2.82", false), template))
			return first, second, err, approvalId
		}
		// with one use left, no session lock between them, the first wins it and the second is spent
		first, _, err, approvalId = lastUse(2)
		if !errors.Is(err, model.ErrApprovalUseSpent) || first.UseIndex != 1 {
			t.Errorf("the last use of two: first %+v, second %v", first, err)
		}
		if rows := approvalScheduleRows(t, ctx, approvalId); len(rows) != 2 {
			t.Errorf("the last use of two: rows %v", rows)
		}
		// twin: with two uses left the second takes the next
		first, second, err, approvalId = lastUse(3)
		if err != nil || first.UseIndex != 1 || second.UseIndex != 2 || second.Replayed {
			t.Errorf("two uses left: %+v %+v %v", first, second, err)
		}
		if rows := approvalScheduleRows(t, ctx, approvalId); len(rows) != 3 {
			t.Errorf("two uses left: rows %v", rows)
		}
	})
}

// §22.7's uses row at the gate: uses = 2 with three actions makes two effects, then 409 SPENT, writing nothing.
func TestFs25TwoUsesThreeActions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, remove := newApprovalScheduleWorld(server.NewId())
		defer remove()
		now := server.NowUtc()
		template := approvalScheduleAction(t, "", true)
		approvalId := sidecar.NewId()
		decision := world.decision(t, template, approvalId, 2, now.Add(time.Hour))
		for index, value := range []string{"192.0.2.90", "192.0.2.91"} {
			outcome, err := world.consume(ctx, world.request(decision, approvalId, approvalScheduleAction(t, value, false), template), now, world.apply)
			if err != nil || outcome.UseIndex != index {
				t.Fatalf("use %d: %+v %v", index, outcome, err)
			}
		}
		_, err := world.consume(ctx, world.request(decision, approvalId, approvalScheduleAction(t, "192.0.2.92", false), template), now, world.apply)
		if approvalScheduleStatus(err) != http.StatusConflict || !errors.Is(err, model.ErrApprovalUseSpent) {
			t.Errorf("a third action: %v", err)
		}
		if rows := approvalScheduleRows(t, ctx, approvalId); len(rows) != 2 || world.effects.Load() != 2 {
			t.Errorf("rows %v, %d effects", rows, world.effects.Load())
		}
	})
}

// §22.7's cancel: after one of three uses the approver's cancel, posted to POST /approval/cancel by a strict_exp bearer
// on §7.4's allowlist, makes the two left cancelled, and no gate spends them; a cancel another member's device signed
// is refused there and inserts nothing. Its twin: with no cancel, the second use spends.
func TestFs25CancelThroughTheRouteRefusesTheRest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancelCtx := context.WithCancel(context.Background())
		defer cancelCtx()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()
		bearer := mintStrictExpBearer(t, self)
		world, remove := newApprovalScheduleWorld(self.networkId)
		defer remove()
		now := server.NowUtc()
		template := approvalScheduleAction(tb, "", true)
		post := func(cancel []byte, decision []byte) (int, int) {
			w := self.serve(http.MethodPost, "/approval/cancel", bearer.authorization(), map[string]any{"cancel": cancel, "decision": decision})
			result := struct {
				CancelledUseCount int `json:"cancelled_use_count"`
			}{}
			if w.Code == http.StatusOK {
				server.Raise(json.Unmarshal(w.Body.Bytes(), &result))
			}
			return w.Code, result.CancelledUseCount
		}

		approvalId := sidecar.NewId()
		decision := world.decision(tb, template, approvalId, 3, now.Add(time.Hour))
		if _, err := world.consume(ctx, world.request(decision, approvalId, approvalScheduleAction(tb, "192.0.2.100", false), template), now, world.apply); err != nil {
			tb.Fatal(err)
		}
		forged := approvalScheduleSigned(tb, world.stranger, bridge.SignedType_SIGNED_APPROVAL_CANCEL, &protocol.ApprovalCancel{ApprovalId: approvalId.Bytes()})
		if status, _ := post(forged, decision); status != http.StatusForbidden {
			tb.Errorf("another member's cancel: status %d", status)
		}
		if rows := approvalScheduleRows(tb, ctx, approvalId); len(rows) != 1 {
			tb.Errorf("a refused cancel wrote %v", rows)
		}
		cancel := approvalScheduleSigned(tb, world.phone, bridge.SignedType_SIGNED_APPROVAL_CANCEL, &protocol.ApprovalCancel{ApprovalId: approvalId.Bytes()})
		if status, count := post(cancel, decision); status != http.StatusOK || count != 2 {
			tb.Fatalf("the cancel: status %d, %d cancelled", status, count)
		}
		if rows := approvalScheduleRows(tb, ctx, approvalId); strings.Join(rows, ",") != "0 done true,1 cancelled false,2 cancelled false" {
			tb.Errorf("rows %v", rows)
		}
		_, err := world.consume(ctx, world.request(decision, approvalId, approvalScheduleAction(tb, "192.0.2.101", false), template), now, world.apply)
		if !errors.Is(err, model.ErrApprovalUseSpent) {
			tb.Errorf("a use after the cancel: %v", err)
		}

		// twin: the same approval shape with no cancel spends its second use
		twinId := sidecar.NewId()
		twin := world.decision(tb, template, twinId, 3, now.Add(time.Hour))
		for index, value := range []string{"192.0.2.102", "192.0.2.103"} {
			outcome, err := world.consume(ctx, world.request(twin, twinId, approvalScheduleAction(tb, value, false), template), now, world.apply)
			if err != nil || outcome.UseIndex != index {
				tb.Errorf("the twin's use %d: %+v %v", index, outcome, err)
			}
		}
	})
}
