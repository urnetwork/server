package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/controller"
)

// stV2Unrecoverable is the operator's explicit command declaring an epoch's
// settlement v2 inputs unrecoverable, so roots list it as providers: null
// (VERSION2-FINAL.md §6.4). The declaration records who made it.
func stV2Unrecoverable(opts docopt.Opts) {
	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}
	by, _ := opts.String("--by")
	if err := controller.StLedgerV2DeclareUnrecoverable(context.Background(), epoch, by); err != nil {
		panic(err)
	}
	fmt.Printf("epoch %d declared unrecoverable by %s\n", epoch, by)
}

// stV2Rebuild is the operator's explicit command naming an epoch whose
// settlement v2 provider rows a later root builds again from v1's inputs
// (VERSION2-FINAL.md §6.4). It records who requested it, and it is refused
// for an epoch a finalized root covers or whose rows are not recorded.
func stV2Rebuild(opts docopt.Opts) {
	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}
	by, _ := opts.String("--by")
	if err := controller.StLedgerV2RebuildEpoch(context.Background(), epoch, by); err != nil {
		panic(err)
	}
	fmt.Printf("epoch %d provider rows to be rebuilt as requested by %s\n", epoch, by)
}

// stV2RebuildWithdraw is the operator's explicit command withdrawing an
// epoch's pending v2-rebuild request, so the driver treats the epoch as if it
// had never been requested (VERSION2-FINAL.md §8.6). It logs and prints who
// made and who withdrew the request, and it is refused for an epoch without a
// pending request.
func stV2RebuildWithdraw(opts docopt.Opts) {
	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}
	by, _ := opts.String("--by")
	requestedBy, err := controller.StLedgerV2WithdrawRebuild(context.Background(), epoch, by)
	if err != nil {
		panic(err)
	}
	fmt.Printf("epoch %d rebuild request by %s withdrawn by %s\n", epoch, requestedBy, by)
}
