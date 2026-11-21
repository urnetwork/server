package model

// §16.6's gate (D-MCP-11, D-MCP-12, I-8, AD4, AU1-AU4, FI-7): approval_use (MG5;
// §2.8 api-10), one row per approval and use, written in the transaction of the
// server-gated effect it consumes: in v0 only §12.8's POST /dns/records, since
// grant activations, §13's publishes included, use grant_id (§8.8).
// ConsumeApprovalUseInTx is the gate, in the caller's one transaction per request:
//
//	1  the use committed under the request's effect key answers it, past the
//	   decision's not_after too, while the row lasts; else §16.6's spend check at
//	   server time (sidecar.SpendableAtGate), whose refusal answers and writes
//	   nothing
//	2  for n in 0 .. uses-1, INSERT ... ON CONFLICT DO NOTHING: the first success
//	   wins use n; with none left, the row a racing request committed for this
//	   effect key answers, else 409 SPENT
//	3  the effect runs in the same transaction, its result stored in the same row
//	4  the caller's COMMIT, the linearization point of a server-gated effect
//
// The keys serialize racing gates, never session locks, which PgBouncer's
// transaction pooling forbids: a racer's INSERT waits on the first's uncommitted
// row and takes nothing once it commits. Under READ COMMITTED the racer then
// takes the next use, or answers the first's row for its own effect key; under
// server.Tx's default REPEATABLE READ its INSERT raises a serialization failure
// instead, and the caller's rerun of its transaction answers from step 1, so a
// caller runs the gate in a transaction that reruns on one, as server.Tx does.
// The caller's transaction is used, never nested (CODESTYLE's locking rule). The request's effect key must be the one
// its action derives (§16.6's effect_key over action_id), so a client cannot
// spend two uses on one action, or replay one action's result for another. The
// gate learns no field it lacked (AD4): the decisions stay in memory, and only
// ids, hashes, state, result and the decision's expiry reach the row (api-10,
// QD-100).
//
// CancelApprovalUses is POST /approval/cancel's: an approver's ApprovalCancel
// with the decision it cancels inserts each use_index no row holds as cancelled,
// with no effect key (QD-503), so a racing gate finds no use left.
// RemoveExpiredApprovalUses is the hourly sweep of rows a day past their
// decision's not_after (QD-100).
//
// The gate judges on the network's verified state as a gate holds it, which the
// api process installs with SetApprovalGateStates (ACL-M2-13's AclGateCache, C-09,
// through ApprovalGateStatesOf). New spends and cancels require a loaded gate.
// MG5 must be linked for every operation, including reading a committed use.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// Tunables of approval_use.
type ApprovalUseSettings struct {
	// How long a row stays past its decision's not_after: a day (§16.6's
	// retention row, §2.8 api-10), after which a retry only reconciles.
	Retention time.Duration
	// The most uses one cancel inserts cancelled rows for: 4096. A decision of
	// more uses is refused at the cancel, since each use is a row (QD-503).
	CancelUseCount int
}

func DefaultApprovalUseSettings() *ApprovalUseSettings {
	return &ApprovalUseSettings{
		Retention:      24 * time.Hour,
		CancelUseCount: 4096,
	}
}

// A row's states (§16.6).
const (
	approvalUseStateDone      = "done"
	approvalUseStateCancelled = "cancelled"
)

const approvalUseUnavailableMessage = "Server-gated approvals are not available yet."

// A refusal of the gate or the cancel: its status, a stable code, and why.
type ApprovalUseError struct {
	Status int
	Code   string
	Err    error
}

func (self *ApprovalUseError) Error() string {
	return fmt.Sprintf("%d %s: %v", self.Status, self.Code, self.Err)
}

func (self *ApprovalUseError) Unwrap() error {
	return self.Err
}

// The gate's refusals.
func approvalUseUnavailable(err error) *ApprovalUseError {
	return &ApprovalUseError{Status: http.StatusServiceUnavailable, Code: "unavailable", Err: err}
}

func approvalUseInvalid(err error) *ApprovalUseError {
	return &ApprovalUseError{Status: http.StatusBadRequest, Code: "invalid", Err: err}
}

func approvalUseRefused(err error) *ApprovalUseError {
	return &ApprovalUseError{Status: http.StatusForbidden, Code: "refused", Err: err}
}

// 409 SPENT: every use of the approval is taken or cancelled (§16.6's gate step 2).
var ErrApprovalUseSpent = errors.New("every use of the approval is spent")

// What one server-gated request presents: its network and bearer's sidecar, the
// decisions and action it carries, its Idempotency-Key, and what the gate's own
// request shows of the action.
type ApprovalUseRequest struct {
	NetworkId server.Id
	// s: the bearer's sidecar, from its client's binding.
	SpenderSidecarId server.Id
	// The Signed ApprovalDecisions as received: one, or a rule's k.
	Decisions [][]byte
	// This use's CanonicalAction as stored.
	Action []byte
	// A multi-use decision's template as stored, which every decision hashes and
	// the action falls inside; nil when the decisions hash the action itself.
	Template []byte
	// Idempotency-Key: §16.6's effect_key of this use.
	EffectKey []byte
	// What the gate's own request shows: host, port, audience, expiry, name and
	// rdata.
	Fields *sidecar.SpendGateFields
	// DNS's zone precondition and configured root are part of the request whose result a retry reads.
	DnsRoot          string
	DnsExpectVersion *uint64
}

// What the gate answered.
type ApprovalUseOutcome struct {
	ApprovalId server.Id
	UseIndex   int
	// The effect's result, as the use's row holds it.
	Result []byte
	// A use committed earlier answered, and no effect ran.
	Replayed bool
}

// The server-gated effect, run in the gate's transaction once use useIndex is
// won: its result, which the use's row stores for a replay. An error rolls the
// caller's transaction back, the use with it.
type ApprovalUseEffect func(ctx context.Context, tx server.PgTx, useIndex int) (result []byte, err error)

// A gate's view of one network's verified state, the spend check's P.
type ApprovalGateStates func(ctx context.Context, networkId server.Id) (sidecar.SpendState, error)

var approvalGateStates atomic.Pointer[ApprovalGateStates]

// Installs where this process's gates read each network's verified state; nil
// removes it, and new spends and cancels answer unavailable.
func SetApprovalGateStates(states ApprovalGateStates) {
	if states == nil {
		approvalGateStates.Store(nil)
		return
	}
	approvalGateStates.Store(&states)
}

// A gate's states from ACL-M2-13's gate cache: the newest verified rows-only
// state of the network, never forked at a gate. Its rows carry the document's
// evidenceDefault (AclRows.evidence_default, QD-727), which an action naming no
// requests rule, a dns write among them, is held to here as at the daemon.
func ApprovalGateStatesOf(cache *sidecar.AclGateCache) ApprovalGateStates {
	return func(ctx context.Context, networkId server.Id) (sidecar.SpendState, error) {
		snapshot, status := cache.State(sidecar.Id(networkId))
		if status != sidecar.AclGateLoaded || snapshot == nil {
			return nil, errors.New("the network's verified state is not loaded")
		}
		return sidecar.NewConfigSpendState(snapshot.Config, false)
	}
}

// The network's state from what this process installed.
func approvalGateState(ctx context.Context, networkId server.Id) (sidecar.SpendState, error) {
	if !server.SidecarMg5Linked() {
		return nil, approvalUseUnavailable(errors.New(approvalUseUnavailableMessage))
	}
	states := approvalGateStates.Load()
	if states == nil {
		return nil, approvalUseUnavailable(errors.New("this process holds no gate states"))
	}
	state, err := (*states)(ctx, networkId)
	if err != nil {
		return nil, approvalUseUnavailable(err)
	}
	return state, nil
}

// §16.6's gate, steps 1 to 3, in tx, the effect's own transaction: the outcome,
// or an *ApprovalUseError (ErrApprovalUseSpent's 409 among them). now is the
// server's time. The caller commits, and a lost response after its commit is
// answered by a retry's step 1.
func ConsumeApprovalUseInTx(ctx context.Context, tx server.PgTx, request *ApprovalUseRequest, now time.Time, apply ApprovalUseEffect) (*ApprovalUseOutcome, error) {
	if request == nil {
		return nil, approvalUseInvalid(errors.New("no request"))
	}
	if !server.SidecarMg5Linked() {
		return nil, approvalUseUnavailable(errors.New(approvalUseUnavailableMessage))
	}
	return consumeApprovalUseInTx(ctx, tx, request, nil, now, apply)
}

// What a request parses to.
type approvalUseParsed struct {
	decisions  []*sidecar.StoredApprovalDecision
	approvalId sidecar.Id
	// this use's action, and what the decisions hash: the template or the action
	use   *sidecar.StoredCanonicalAction
	bound *sidecar.StoredCanonicalAction
}

// The request's decisions and actions, parsed from their bytes as received, its
// effect key the one its action derives.
func parseApprovalUseRequest(request *ApprovalUseRequest) (*approvalUseParsed, error) {
	if len(request.Decisions) == 0 {
		return nil, approvalUseInvalid(errors.New("no decision"))
	}
	parsed := &approvalUseParsed{}
	for _, signedBytes := range request.Decisions {
		decision, err := sidecar.ParseStoredApprovalDecision(signedBytes)
		if err != nil {
			return nil, approvalUseInvalid(err)
		}
		parsed.decisions = append(parsed.decisions, decision)
	}
	approvalId, err := sidecar.IdFromBytes(parsed.decisions[0].Decision.GetApprovalId())
	if err != nil {
		return nil, approvalUseInvalid(err)
	}
	parsed.approvalId = approvalId
	use, err := sidecar.ParseStoredCanonicalAction(request.Action)
	if err != nil {
		return nil, approvalUseInvalid(err)
	}
	parsed.use, parsed.bound = use, use
	if request.Template != nil {
		template, err := sidecar.ParseStoredCanonicalAction(request.Template)
		if err != nil {
			return nil, approvalUseInvalid(err)
		}
		if !sidecar.ActionMatchesTemplate(template, use) {
			return nil, approvalUseInvalid(errors.New("the action does not fall inside its template"))
		}
		parsed.bound = template
	}
	effectKey := sidecar.ApprovalEffectKey(approvalId, use.ActionId(approvalId))
	if !bytes.Equal(request.EffectKey, effectKey[:]) {
		return nil, approvalUseInvalid(errors.New("the Idempotency-Key is not this use's effect key"))
	}
	return parsed, nil
}

// The spend check at server time: the gate clause on this use's own action, then
// the decisions as a set on what they hash, allowing no skew (§16.6's expiry row).
func approvalGateSpend(parsed *approvalUseParsed, request *ApprovalUseRequest, state sidecar.SpendState, now time.Time) (*sidecar.SpendBudget, error) {
	input := &sidecar.SpendInput{Action: parsed.bound, Spender: sidecar.Id(request.SpenderSidecarId), Now: now}
	if parsed.bound == parsed.use {
		return sidecar.SpendableAtGate(parsed.decisions, input, request.Fields, state)
	}
	if err := sidecar.CheckCanonicalActionForServer(parsed.bound); err != nil {
		return nil, err
	}
	if err := sidecar.CheckSpendGateFields(parsed.use, request.Fields); err != nil {
		return nil, err
	}
	settings := sidecar.DefaultSpendSettings()
	// a gate judges on its own clock, which allows none
	settings.DecisionSkew = 0
	return sidecar.SpendableSet(parsed.decisions, input, state, settings)
}

// A done result authenticates the wire decisions and the route's independently authenticated caller and fields.
// The gate keeps only the result with its authenticator. Neither decisions nor DNS data are copied into the row.
const approvalUseReceiptV1 = "\x00ur-approval-result-v1\x00"

// Key for a result's authenticator. The sequence is the v1 format; each decision includes its signature.
func approvalUseReceiptKey(request *ApprovalUseRequest) ([]byte, error) {
	fields := request.Fields
	hash := sha256.New()
	err := json.NewEncoder(hash).Encode([]any{approvalUseReceiptV1, request.NetworkId, request.SpenderSidecarId,
		request.Decisions, request.Action, request.Template, request.EffectKey, fields.Kind, fields.Host, fields.Port,
		fields.Audience, fields.ExpiresMillis, fields.DnsName, fields.DnsType, fields.DnsValue, fields.DnsTtl,
		request.DnsRoot, request.DnsExpectVersion})
	return hash.Sum(nil), err
}

// The authenticator of committed result bytes, under the request the gate originally verified.
func approvalUseResultTag(key []byte, result []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(result)
	return mac.Sum(nil)
}

// DNS is v0's only production effect. Its receipt is also a valid DNS answer so a rolling older
// gate, which returns the stored bytes directly, returns exactly the first response to the client.
type approvalUseDnsReceipt struct {
	sidecar.DnsRecordsAnswer
	Receipt []byte `json:"approval_receipt_v1"`
}

func storeApprovalUseResult(request *ApprovalUseRequest, key []byte, result []byte) (stored []byte, answer []byte, err error) {
	if request.DnsRoot != "" {
		var dns approvalUseDnsReceipt
		if err := json.Unmarshal(result, &dns.DnsRecordsAnswer); err != nil {
			return nil, nil, err
		}
		body, err := json.Marshal(dns.DnsRecordsAnswer)
		if err != nil {
			return nil, nil, err
		}
		dns.Receipt = approvalUseResultTag(key, body)
		stored, err = json.Marshal(dns)
		return stored, stored, err
	}
	// Other callers can have an opaque (non-JSON) result.
	stored = bytes.Join([][]byte{[]byte(approvalUseReceiptV1), approvalUseResultTag(key, result), result}, nil)
	return stored, result, nil
}

// Reads a committed use first. Only a first spend or a legacy result needs the current projection.
func consumeApprovalUseInTx(ctx context.Context, tx server.PgTx, request *ApprovalUseRequest, state sidecar.SpendState, now time.Time, apply ApprovalUseEffect) (*ApprovalUseOutcome, error) {
	parsed, err := parseApprovalUseRequest(request)
	if err != nil {
		return nil, err
	}
	approvalId := server.Id(parsed.approvalId)
	if err := sidecar.CheckSpendGateFields(parsed.use, request.Fields); err != nil {
		return nil, approvalUseRefused(err)
	}
	receiptKey, err := approvalUseReceiptKey(request)
	if err != nil {
		return nil, err
	}
	spend := func(at time.Time) (*sidecar.SpendBudget, error) {
		if state == nil {
			var err error
			state, err = approvalGateState(ctx, request.NetworkId)
			if err != nil {
				return nil, err
			}
		}
		budget, err := approvalGateSpend(parsed, request, state, at)
		if err != nil {
			return nil, approvalUseRefused(err)
		}
		return budget, nil
	}
	lookup := func() (*ApprovalUseOutcome, bool, error) {
		outcome, at, found, err := selectApprovalUseInTx(ctx, tx, request.NetworkId, approvalId, request.EffectKey)
		if err != nil || !found {
			return outcome, found, err
		}
		if stored, ok := bytes.CutPrefix(outcome.Result, []byte(approvalUseReceiptV1)); ok {
			if len(stored) < sha256.Size || !hmac.Equal(stored[:sha256.Size], approvalUseResultTag(receiptKey, stored[sha256.Size:])) {
				return nil, true, approvalUseRefused(errors.New("the request differs from the committed use"))
			}
			outcome.Result = stored[sha256.Size:]
		} else {
			var dns approvalUseDnsReceipt
			if json.Unmarshal(outcome.Result, &dns) == nil && dns.Receipt != nil {
				body, err := json.Marshal(dns.DnsRecordsAnswer)
				if err != nil || !hmac.Equal(dns.Receipt, approvalUseResultTag(receiptKey, body)) {
					return nil, true, approvalUseRefused(errors.New("the request differs from the committed use"))
				}
			} else if _, err := spend(at); err != nil {
				// Earlier releases kept only the raw result. Verify the spend at its commit time before disclosing it.
				return nil, true, err
			}
		}
		return outcome, true, nil
	}

	// 1: an authenticated retry returns its committed result before reading current state.
	if outcome, found, err := lookup(); err != nil || found {
		return outcome, err
	}
	budget, err := spend(now)
	if err != nil {
		return nil, err
	}
	// 2: the first free use this request inserts is its own; a use another request
	// takes after this read is its insert's conflict, which skips to the next
	taken, err := takenApprovalUsesInTx(ctx, tx, approvalId)
	if err != nil {
		return nil, err
	}
	approvalUseClaimHook(ctx)
	notAfter := time.UnixMilli(budget.NotAfterMillis).UTC()
	useIndex := -1
	for n := 0; n < int(budget.Uses); n += 1 {
		if slices.Contains(taken, n) {
			continue
		}
		tag, err := tx.Exec(
			ctx,
			`
				INSERT INTO approval_use (
					network_id,
					approval_id,
					use_index,
					effect_key,
					state,
					create_time,
					not_after
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT DO NOTHING
			`,
			request.NetworkId,
			approvalId,
			n,
			request.EffectKey,
			approvalUseStateDone,
			now.UTC(),
			notAfter,
		)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			useIndex = n
			break
		}
	}
	if useIndex < 0 {
		// none left: a racer that committed this effect key answers, else spent
		if outcome, found, err := lookup(); err != nil || found {
			return outcome, err
		}
		return nil, &ApprovalUseError{Status: http.StatusConflict, Code: "spent", Err: ErrApprovalUseSpent}
	}

	// 3: the effect, and its result in the same row
	result, err := apply(ctx, tx, useIndex)
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = []byte{}
	}
	stored, answer, err := storeApprovalUseResult(request, receiptKey, result)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		ctx,
		`
			UPDATE approval_use
			SET result = $3
			WHERE approval_id = $1 AND use_index = $2
		`,
		approvalId,
		useIndex,
		stored,
	); err != nil {
		return nil, err
	}
	return &ApprovalUseOutcome{ApprovalId: approvalId, UseIndex: useIndex, Result: answer}, nil
}

// The done use committed under effectKey, replayed, and false for none.
func selectApprovalUseInTx(ctx context.Context, tx server.PgTx, networkId server.Id, approvalId server.Id, effectKey []byte) (*ApprovalUseOutcome, time.Time, bool, error) {
	var useIndex int
	var result []byte
	var created time.Time
	err := tx.QueryRow(
		ctx,
		`
			SELECT use_index, result, create_time
			FROM approval_use
			WHERE approval_id = $1 AND effect_key = $2 AND network_id = $3 AND state = $4
		`,
		approvalId,
		effectKey,
		networkId,
		approvalUseStateDone,
	).Scan(&useIndex, &result, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, created, false, nil
	}
	if err != nil {
		return nil, created, false, err
	}
	if result == nil {
		result = []byte{}
	}
	return &ApprovalUseOutcome{ApprovalId: approvalId, UseIndex: useIndex, Result: result, Replayed: true}, created, true, nil
}

// The use indexes rows hold for the approval, done and cancelled.
func takenApprovalUsesInTx(ctx context.Context, tx server.PgTx, approvalId server.Id) ([]int, error) {
	rows, err := tx.Query(
		ctx,
		`
			SELECT use_index
			FROM approval_use
			WHERE approval_id = $1
		`,
		approvalId,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	taken := []int{}
	for rows.Next() {
		var useIndex int
		if err := rows.Scan(&useIndex); err != nil {
			return nil, err
		}
		taken = append(taken, useIndex)
	}
	return taken, rows.Err()
}

// The test seam between the spend check and the claim, nil outside a test.
type approvalUseClaimHookKey struct{}

// Runs the context's claim hook: a test holds one request there while another
// commits, as two api instances race.
func approvalUseClaimHook(ctx context.Context) {
	if hook, ok := ctx.Value(approvalUseClaimHookKey{}).(func()); ok {
		hook()
	}
}

// POST /approval/cancel's: an approver's Signed ApprovalCancel and the Signed
// decision it cancels, both as received. The cancel's signer must be the
// decision's approver (sidecar.CheckApprovalCancel); then each use_index no row
// holds becomes a cancelled row with no effect key and the decision's not_after
// (QD-503). The count it inserted; a use a gate committed first stays done.
func CancelApprovalUses(ctx context.Context, networkId server.Id, cancelSigned []byte, decisionSigned []byte, now time.Time, settings *ApprovalUseSettings) (int, error) {
	state, err := approvalGateState(ctx, networkId)
	if err != nil {
		return 0, err
	}
	return cancelApprovalUses(ctx, networkId, cancelSigned, decisionSigned, state, now, settings)
}

// CancelApprovalUses on state.
func cancelApprovalUses(ctx context.Context, networkId server.Id, cancelSigned []byte, decisionSigned []byte, state sidecar.SpendState, now time.Time, settings *ApprovalUseSettings) (int, error) {
	if settings == nil {
		settings = DefaultApprovalUseSettings()
	}
	decision, err := sidecar.ParseStoredApprovalDecision(decisionSigned)
	if err != nil {
		return 0, approvalUseInvalid(err)
	}
	cancel := &bridge.Signed{}
	if err := proto.Unmarshal(cancelSigned, cancel); err != nil {
		return 0, approvalUseInvalid(err)
	}
	if err := sidecar.CheckApprovalCancel(cancel, decision, state); err != nil {
		return 0, approvalUseRefused(err)
	}
	uses := int(decision.Decision.GetUses())
	if settings.CancelUseCount < uses {
		return 0, approvalUseInvalid(fmt.Errorf("a decision of %d uses, past %d", uses, settings.CancelUseCount))
	}
	decisionApprovalId, err := sidecar.IdFromBytes(decision.Decision.GetApprovalId())
	if err != nil {
		return 0, approvalUseInvalid(err)
	}
	approvalId := server.Id(decisionApprovalId)
	notAfter := time.UnixMilli(decision.Decision.GetNotAfterMillis()).UTC()
	inserted := 0
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO approval_use (
					network_id,
					approval_id,
					use_index,
					effect_key,
					state,
					create_time,
					not_after
				)
				SELECT $1, $2, use_index, NULL, $3, $4, $5
				FROM generate_series(0, $6 - 1) AS use_index
				ON CONFLICT DO NOTHING
			`,
			networkId,
			approvalId,
			approvalUseStateCancelled,
			now.UTC(),
			notAfter,
			uses,
		))
		inserted = int(tag.RowsAffected())
	})
	return inserted, nil
}

// Deletes the rows whose decision's not_after is before minNotAfter: what the
// hourly sweep runs with now less Retention. A no-op without MG5.
func RemoveExpiredApprovalUses(ctx context.Context, minNotAfter time.Time) (removedCount int64) {
	if !server.SidecarMg5Linked() {
		return 0
	}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM approval_use
				WHERE not_after < $1
			`,
			minNotAfter.UTC(),
		))
		removedCount = tag.RowsAffected()
	})
	return
}
