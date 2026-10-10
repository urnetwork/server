// Discovery cancellation preserves typed source custody at both the unstarted
// cursor and completed-prefix seams. Tests wait on the actual owned deadline.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An interrupted first source seek must not become EOF while a dense paid
// round remains active. Its serialized empty cursor retries begin next time.
func TestContractCloseOwnerSourceDeadlineBeforeBeginRetainsUnstartedCursor(t *testing.T) {
	ctx := t.Context()
	bounded, cancel := context.WithTimeoutCause(ctx, legacySettlementDispatchBudget, errLegacySettlementDispatchBudget)
	defer cancel()
	passEnd := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	prefix := server.NewId()
	payerCursor := &LegacySettlementPayerCursor{
		End: legacyPayerTestContractId(prefix, uint32(legacySettlementPayerProbeLimit), 0), PassEndTime: passEnd,
	}
	var discoveryContext context.Context
	source := &legacySourceOwnerDispatch{
		begin: func(callCtx context.Context, shard int) *LegacySettlementPayerCursor {
			discoveryContext = callCtx
			<-callCtx.Done()
			server.Raise(callCtx.Err())
			return nil
		},
		next: func(context.Context, int, *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
			t.Fatal("interrupted source begin advanced into a probe")
			return nil, nil
		},
	}
	payerProbes, registrations := 0, 0
	result, err := dispatchLegacySettlementPayersPage(ctx, bounded, 0, nil, payerCursor,
		func(context.Context, int, *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
			payerProbes++
			payer := legacyPayerTestContractId(prefix, uint32(payerProbes), 0)
			return &payer, nil
		}, func(callCtx context.Context, _ int, after *LegacySettlementCursor) (*LegacySettlementCursor, int) {
			registrations++
			if callCtx.Err() != nil || bounded.Err() != nil {
				t.Fatal("source child deadline consumed the registration allowance")
			}
			return after, 0
		}, source)
	if err != nil || discoveryContext == nil || context.Cause(discoveryContext) != errLegacySettlementDiscoveryBudget ||
		!source.yielded || source.cursor == nil || source.cursor.End != (server.Id{}) || source.cursor.After != nil ||
		source.probes != 0 || len(source.ids) != 0 || registrations != 1 || result.RegistrationFailed ||
		result.PayerCursor == nil || result.Probes != legacySettlementPayerProbeLimit || payerCursor.After != nil {
		t.Fatal("source first-seek deadline was mistaken for a completed family", source, result, err)
	}
	encoded, err := json.Marshal(source.cursor)
	server.Raise(err)
	var continued LegacySettlementPayerCursor
	server.Raise(json.Unmarshal(encoded, &continued))
	ownerId, contractId := server.NewId(), server.NewId()
	begins, probes := 0, 0
	resume := &legacySourceOwnerDispatch{
		cursor: &continued,
		begin: func(context.Context, int) *LegacySettlementPayerCursor {
			begins++
			return &LegacySettlementPayerCursor{End: ownerId, PassEndTime: passEnd}
		},
		next: func(context.Context, int, *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
			probes++
			if probes == 1 {
				return &ownerId, &LegacySettlementPosition{NextAttemptTime: passEnd, ContractId: contractId}
			}
			return nil, nil
		},
		matches: func(_ context.Context, id server.Id, owner ContractCloseOwner) bool {
			return id == contractId && owner == (ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: ownerId})
		},
	}
	if err := resume.discover(ctx, 0); err != nil || begins != 1 || probes != 2 || resume.cursor != nil || len(resume.ids) != 1 || resume.ids[0] != ownerId {
		t.Fatal("serialized unstarted source cursor did not resume its actual family", resume, begins, probes, err)
	}
}

// A source deadline after one accepted owner keeps exactly that prefix and
// resumes after it. Registration still runs within the original outer turn.
func TestContractCloseOwnerSourceDeadlineRetainsCompletedPrefix(t *testing.T) {
	ctx := t.Context()
	bounded, cancel := context.WithTimeoutCause(ctx, legacySettlementDispatchBudget, errLegacySettlementDispatchBudget)
	defer cancel()
	passEnd := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	prefix := server.NewId()
	first := legacyPayerTestContractId(prefix, 1, 0)
	second := legacyPayerTestContractId(prefix, 2, 0)
	firstContractId, secondContractId := server.NewId(), server.NewId()
	input := &LegacySettlementPayerCursor{End: second, PassEndTime: passEnd}
	copyCursor := *input
	probes, registrations := 0, 0
	source := &legacySourceOwnerDispatch{
		cursor: &copyCursor,
		next: func(callCtx context.Context, _ int, _ *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
			probes++
			if probes == 1 {
				return &first, &LegacySettlementPosition{NextAttemptTime: passEnd, ContractId: firstContractId}
			}
			<-callCtx.Done()
			server.Raise(callCtx.Err())
			return nil, nil
		},
		matches: func(_ context.Context, id server.Id, owner ContractCloseOwner) bool {
			return id == firstContractId && owner == (ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: first})
		},
	}
	result, err := dispatchLegacySettlementPayersPage(ctx, bounded, 0, nil,
		&LegacySettlementPayerCursor{End: server.NewId(), PassEndTime: passEnd},
		func(context.Context, int, *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
			return nil, nil
		}, func(callCtx context.Context, _ int, after *LegacySettlementCursor) (*LegacySettlementCursor, int) {
			registrations++
			if callCtx.Err() != nil || bounded.Err() != nil {
				t.Fatal("source prefix deadline canceled registration")
			}
			return after, 0
		}, source)
	if err != nil || !source.yielded || probes != 2 || source.probes != 1 || len(source.ids) != 1 || source.ids[0] != first ||
		source.cursor == nil || source.cursor.After == nil || *source.cursor.After != first || source.cursor.End != second ||
		!source.cursor.PassEndTime.Equal(passEnd) || input.After != nil || registrations != 1 || result.RegistrationFailed {
		t.Fatal("source deadline lost the exact accepted prefix or mutated its input", source, result, err)
	}
	encoded, err := json.Marshal(source.cursor)
	server.Raise(err)
	var continued LegacySettlementPayerCursor
	server.Raise(json.Unmarshal(encoded, &continued))
	resumeProbes := 0
	resume := &legacySourceOwnerDispatch{
		cursor: &continued,
		begin: func(context.Context, int) *LegacySettlementPayerCursor {
			t.Fatal("completed source prefix restarted its fixed round")
			return nil
		},
		next: func(_ context.Context, _ int, cursor *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
			resumeProbes++
			if resumeProbes == 1 {
				if cursor.After == nil || *cursor.After != first {
					t.Fatal("source resume did not start after its accepted prefix", cursor)
				}
				return &second, &LegacySettlementPosition{NextAttemptTime: passEnd, ContractId: secondContractId}
			}
			return nil, nil
		},
		matches: func(_ context.Context, id server.Id, owner ContractCloseOwner) bool {
			return id == secondContractId && owner == (ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: second})
		},
	}
	if err := resume.discover(ctx, 0); err != nil || resume.cursor != nil || resumeProbes != 2 || len(resume.ids) != 1 || resume.ids[0] != second {
		t.Fatal("source continuation duplicated its prefix or missed its successor", resume, err)
	}
}

// Foreign cancellation never becomes a successful discovery yield, even
// when the source family has already found a valid publishable owner.
func TestContractCloseOwnerSourceParentCancellationRemainsError(t *testing.T) {
	ctx, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	bounded, cancel := context.WithTimeoutCause(ctx, legacySettlementDispatchBudget, errLegacySettlementDispatchBudget)
	defer cancel()
	passEnd := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
	ownerId, contractId := server.NewId(), server.NewId()
	probes, registrations := 0, 0
	source := &legacySourceOwnerDispatch{
		cursor: &LegacySettlementPayerCursor{End: ownerId, PassEndTime: passEnd},
		next: func(callCtx context.Context, _ int, _ *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
			probes++
			if probes == 1 {
				return &ownerId, &LegacySettlementPosition{NextAttemptTime: passEnd, ContractId: contractId}
			}
			parentCancel()
			server.Raise(callCtx.Err())
			return nil, nil
		},
		matches: func(_ context.Context, id server.Id, owner ContractCloseOwner) bool {
			return id == contractId && owner == (ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: ownerId})
		},
	}
	result, err := dispatchLegacySettlementPayersPage(ctx, bounded, 0, nil,
		&LegacySettlementPayerCursor{End: server.NewId(), PassEndTime: passEnd},
		func(context.Context, int, *LegacySettlementPayerCursor) (*server.Id, *LegacySettlementPosition) {
			return nil, nil
		}, func(context.Context, int, *LegacySettlementCursor) (*LegacySettlementCursor, int) {
			registrations++
			return nil, 0
		}, source)
	if !errors.Is(err, context.Canceled) || source.yielded || registrations != 0 || result.More ||
		probes != 2 || source.probes != 1 || len(source.ids) != 1 || source.ids[0] != ownerId ||
		source.cursor == nil || source.cursor.After == nil || *source.cursor.After != ownerId {
		t.Fatal("parent cancellation became successful source discovery or lost its prefix", source, result, err)
	}
}
