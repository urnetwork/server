package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/nativefee"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func nativeFeeCLIArguments() []string {
	return []string{"settle", "--intent", "11111111-1111-1111-1111-111111111111", "--request", "/retained/request.json", "--request-sha256", "sha256:" + strings.Repeat("21", 32), "--transaction", "0x" + strings.Repeat("22", 32), "--budget", "2m"}
}

// The command fixture only observes dispatch; it supplies no native authority.
func TestNativeFeeCLIForwardsOnlyOriginalSelection(t *testing.T) {
	var output bytes.Buffer
	called := false
	err := run(t.Context(), nativeFeeCLIArguments(), &output, func(ctx context.Context, intent server.Id, request nativefee.Reference, transaction string, budget time.Duration) (*model.StTransactionNativeFeeSettlement, error) {
		called = true
		if _, ok := ctx.Deadline(); !ok || budget != 2*time.Minute || intent.String() != "11111111-1111-1111-1111-111111111111" || request.Path != "/retained/request.json" || transaction != "0x"+strings.Repeat("22", 32) {
			t.Fatal("command changed original proof selection")
		}
		return &model.StTransactionNativeFeeSettlement{IntentId: intent, TransactionHash: transaction, DebitRao: "750", DebitWei: "750"}, nil
	})
	if err != nil || !called || output.Len() == 0 {
		t.Fatal("bounded command did not reach settlement owner", err)
	}
}

func TestNativeFeeCLIRejectsImportedReportOption(t *testing.T) {
	var output bytes.Buffer
	called := false
	err := run(t.Context(), append(nativeFeeCLIArguments(), "--report", "/caller/forged.json"), &output, func(context.Context, server.Id, nativefee.Reference, string, time.Duration) (*model.StTransactionNativeFeeSettlement, error) {
		called = true
		return nil, nil
	})
	if err == nil || called || output.Len() != 0 {
		t.Fatal("caller-selected report reached settlement")
	}
}

func TestNativeFeeCLIProofFailurePublishesNoSettlement(t *testing.T) {
	var output bytes.Buffer
	unknown := errors.New("original refund unknown")
	err := run(t.Context(), nativeFeeCLIArguments(), &output, func(context.Context, server.Id, nativefee.Reference, string, time.Duration) (*model.StTransactionNativeFeeSettlement, error) {
		return nil, unknown
	})
	if !errors.Is(err, unknown) || output.Len() != 0 {
		t.Fatal("unknown proof published a settlement", err)
	}
}

func TestNativeFeeCLICanceledOwnerDoesNotInvoke(t *testing.T) {
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := run(ctx, nativeFeeCLIArguments(), &output, func(context.Context, server.Id, nativefee.Reference, string, time.Duration) (*model.StTransactionNativeFeeSettlement, error) {
		called = true
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) || called || output.Len() != 0 {
		t.Fatal("canceled command dispatched verification", err)
	}
}
