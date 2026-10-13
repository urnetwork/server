package model

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/urnetwork/server"
)

// §6.4 (fixed inputs): a rebuild request names who made it and leaves the
// record as it is; an epoch without a record, or another pool's, is not
// requested; rows recorded for a requested epoch replace its rows or its null
// declaration and end the request, while a declaration never replaces it and
// a record without a request still refuses other rows.
func TestStLedgerV2EpochRebuildRequests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		chunks, other := []string{testLedgerV2Ref(1)}, []string{testLedgerV2Ref(2), testLedgerV2Ref(3)}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 5, chunks); err != nil {
			t.Fatal(err)
		}
		if err := DeclareStLedgerV2EpochUnrecoverable(ctx, key, 1, 6, "operator@example"); err != nil {
			t.Fatal(err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 2, 5, chunks); err != nil {
			t.Fatal(err)
		}

		if requested, err := RequestStLedgerV2EpochRebuild(ctx, key, 1, 5, ""); requested || err == nil || err.Error() != "st ledger v2: a rebuild request must name who makes it" {
			t.Fatalf("anonymous request: %t %v", requested, err)
		}
		for _, epoch := range []uint64{5, 6} {
			if requested, err := RequestStLedgerV2EpochRebuild(ctx, key, 1, epoch, "rebuilder@example"); !requested || err != nil {
				t.Fatalf("request epoch %d: %t %v", epoch, requested, err)
			}
		}
		if requested, err := RequestStLedgerV2EpochRebuild(ctx, key, 1, 7, "rebuilder@example"); requested || err != nil {
			t.Fatalf("request of an unrecorded epoch: %t %v", requested, err)
		}
		records := GetStLedgerV2EpochRows(ctx, key, 1, 5, 7)
		if r := records[5]; len(records) != 2 || !reflect.DeepEqual(r.ProviderChunks, chunks) || r.RebuildRequestedBy != "rebuilder@example" {
			t.Fatalf("requested epoch 5 %+v", records)
		}
		if r := records[6]; !r.Null() || r.DeclaredBy != "operator@example" || r.RebuildRequestedBy != "rebuilder@example" {
			t.Fatalf("requested epoch 6 %+v", r)
		}
		if r := GetStLedgerV2EpochRows(ctx, key, 2, 5, 5)[5]; r.RebuildRequestedBy != "" {
			t.Fatalf("another pool's epoch was requested: %+v", r)
		}

		// a declaration never replaces a requested record
		if err := DeclareStLedgerV2EpochUnrecoverable(ctx, key, 1, 5, "operator@example"); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("declaration over requested rows: %v", err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 5, other); err != nil {
			t.Fatal(err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 6, nil); err != nil {
			t.Fatal(err)
		}
		records = GetStLedgerV2EpochRows(ctx, key, 1, 5, 6)
		if r := records[5]; !reflect.DeepEqual(r.ProviderChunks, other) || r.RebuildRequestedBy != "" || r.Null() {
			t.Fatalf("rebuilt epoch 5 %+v", r)
		}
		if r := records[6]; r.Null() || r.ProviderChunks == nil || len(r.ProviderChunks) != 0 || r.DeclaredBy != "" || r.RebuildRequestedBy != "" {
			t.Fatalf("rebuilt epoch 6 %+v", r)
		}
		// the request ended: the rebuilt rows are fixed again
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 5, chunks); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("rows over rebuilt rows: %v", err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 2, 5, other); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("rows over another pool's unrequested rows: %v", err)
		}
	})
}

// §6.4 (fixed inputs), §8.6: withdrawing the requests read for an epoch's rows
// and for an epoch's null declaration leaves both records exactly as they
// were before the requests, so rows built for a request no longer replace
// them, and leaves the same epoch's requests in another pool and another
// deployment.
func TestStLedgerV2EpochRebuildWithdrawalRestoresTheRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key, other := testStDeploymentKey, StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		chunks, rebuilt := []string{testLedgerV2Ref(1)}, []string{testLedgerV2Ref(2)}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 5, chunks); err != nil {
			t.Fatal(err)
		}
		if err := DeclareStLedgerV2EpochUnrecoverable(ctx, key, 1, 6, "operator@example"); err != nil {
			t.Fatal(err)
		}
		for _, scope := range []struct {
			key  StDeploymentKey
			noId uint64
		}{{key, 2}, {other, 1}} {
			if err := RecordStLedgerV2EpochRows(ctx, scope.key, scope.noId, 5, chunks); err != nil {
				t.Fatal(err)
			}
			if requested, err := RequestStLedgerV2EpochRebuild(ctx, scope.key, scope.noId, 5, "rebuilder@example"); !requested || err != nil {
				t.Fatalf("request %s pool %d: %t %v", scope.key, scope.noId, requested, err)
			}
		}
		recorded := GetStLedgerV2EpochRows(ctx, key, 1, 5, 6)
		for _, epoch := range []uint64{5, 6} {
			if requested, err := RequestStLedgerV2EpochRebuild(ctx, key, 1, epoch, "rebuilder@example"); !requested || err != nil {
				t.Fatalf("request epoch %d: %t %v", epoch, requested, err)
			}
		}

		requested := GetStLedgerV2EpochRows(ctx, key, 1, 5, 6)
		for _, epoch := range []uint64{5, 6} {
			if !WithdrawStLedgerV2EpochRebuild(ctx, key, 1, requested[epoch]) {
				t.Fatalf("epoch %d's request %+v was not withdrawn", epoch, requested[epoch])
			}
		}
		if got := GetStLedgerV2EpochRows(ctx, key, 1, 5, 6); !reflect.DeepEqual(got, recorded) {
			t.Fatalf("withdrawn records %+v %+v, recorded %+v %+v", got[5], got[6], recorded[5], recorded[6])
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 5, rebuilt); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("rows over withdrawn rows: %v", err)
		}
		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 6, rebuilt); !errors.Is(err, ErrStLedgerV2EpochRowsFixed) {
			t.Fatalf("rows over a withdrawn declaration: %v", err)
		}
		if got := GetStLedgerV2EpochRows(ctx, key, 1, 5, 6); !reflect.DeepEqual(got, recorded) {
			t.Fatalf("refused rows changed the records: %+v %+v", got[5], got[6])
		}
		if r := GetStLedgerV2EpochRows(ctx, key, 2, 5, 5)[5]; r.RebuildRequestedBy != "rebuilder@example" {
			t.Fatalf("another pool's request was withdrawn: %+v", r)
		}
		if r := GetStLedgerV2EpochRows(ctx, other, 1, 5, 5)[5]; r.RebuildRequestedBy != "rebuilder@example" {
			t.Fatalf("another deployment's request was withdrawn: %+v", r)
		}
	})
}

// §6.4 (fixed inputs), §8.6: the withdrawal is a compare-and-set on the
// request read, so it withdraws nothing once that request has ended: when it
// was withdrawn already, or when rows rebuilt for it since replaced the
// record, even when the same operator has requested the rebuilt rows again.
func TestStLedgerV2EpochRebuildWithdrawalRefusesARequestEndedSinceItsRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		chunks, rebuilt := []string{testLedgerV2Ref(1)}, []string{testLedgerV2Ref(2)}
		for _, epoch := range []uint64{5, 6} {
			if err := RecordStLedgerV2EpochRows(ctx, key, 1, epoch, chunks); err != nil {
				t.Fatal(err)
			}
			if requested, err := RequestStLedgerV2EpochRebuild(ctx, key, 1, epoch, "rebuilder@example"); !requested || err != nil {
				t.Fatalf("request epoch %d: %t %v", epoch, requested, err)
			}
		}
		read := GetStLedgerV2EpochRows(ctx, key, 1, 5, 6)

		if !WithdrawStLedgerV2EpochRebuild(ctx, key, 1, read[5]) {
			t.Fatal("epoch 5's request was not withdrawn")
		}
		withdrawn := GetStLedgerV2EpochRows(ctx, key, 1, 5, 5)[5]
		if WithdrawStLedgerV2EpochRebuild(ctx, key, 1, read[5]) {
			t.Fatal("a withdrawn request was withdrawn again")
		}
		if got := GetStLedgerV2EpochRows(ctx, key, 1, 5, 5)[5]; !reflect.DeepEqual(got, withdrawn) {
			t.Fatalf("withdrawn record changed: %+v", got)
		}

		if err := RecordStLedgerV2EpochRows(ctx, key, 1, 6, rebuilt); err != nil {
			t.Fatal(err)
		}
		if requested, err := RequestStLedgerV2EpochRebuild(ctx, key, 1, 6, "rebuilder@example"); !requested || err != nil {
			t.Fatalf("request the rebuilt rows: %t %v", requested, err)
		}
		requestedAgain := GetStLedgerV2EpochRows(ctx, key, 1, 6, 6)[6]
		if WithdrawStLedgerV2EpochRebuild(ctx, key, 1, read[6]) {
			t.Fatal("a request ended by rebuilt rows was withdrawn")
		}
		if !reflect.DeepEqual(requestedAgain.ProviderChunks, rebuilt) || requestedAgain.RebuildRequestedBy != "rebuilder@example" || requestedAgain.CreateTime.Equal(read[6].CreateTime) {
			t.Fatalf("rebuilt and requested again %+v, read %+v", requestedAgain, read[6])
		}
		if got := GetStLedgerV2EpochRows(ctx, key, 1, 6, 6)[6]; !reflect.DeepEqual(got, requestedAgain) {
			t.Fatalf("the later request changed: %+v", got)
		}
	})
}
