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
