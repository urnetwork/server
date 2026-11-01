package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
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

// stV2RecoveryUsage preserves a scheduled recovery source epoch's usage
// census (VERSION2-FINAL.md §16 step 6) as a content-addressed artifact: a
// manifest and its usage chunks, written into a new --out directory and,
// with --blob, put once each in the blob store's private recovery prefix.
// The process is read-only: PostgreSQL refuses every write it attempts, and
// the census is one REPEATABLE READ, READ ONLY transaction through the
// whole-window reader. Standard output carries counts and references only.
func stV2RecoveryUsage(opts docopt.Opts) {
	server.RequirePgReadOnly()
	ctx := context.Background()
	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}
	out, _ := opts.String("--out")
	// fail before the census, not after it
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		panic(fmt.Errorf("--out %q already exists or cannot be checked: %v", out, err))
	}
	store, _ := opts.Bool("--blob")
	start := time.Now()
	usage, err := controller.BuildStLedgerV2RecoveryUsage(ctx, epoch)
	if err != nil {
		panic(err)
	}
	if err := controller.WriteStLedgerV2RecoveryUsage(usage, out); err != nil {
		panic(err)
	}
	if store {
		if err := controller.StoreStLedgerV2RecoveryUsage(ctx, usage); err != nil {
			panic(err)
		}
	}
	writeStV2RecoveryUsageSummary(os.Stdout, usage, out, store, time.Since(start))
}

func writeStV2RecoveryUsageSummary(w io.Writer, usage *controller.StLedgerV2RecoveryUsage, out string, stored bool, took time.Duration) {
	manifest := usage.Manifest
	fmt.Fprintln(w, "st v2 recovery usage: read-only census; nothing was recorded or sent")
	fmt.Fprintf(w, "%-24s %d (pool %d, deployment %s)\n", "epoch", manifest.Epoch, manifest.NoId, manifest.DeploymentId)
	fmt.Fprintf(w, "%-24s blocks %d to %d, %s to %s\n", "window", manifest.Start.Block, manifest.End.Block, manifest.Start.Time, manifest.End.Time)
	fmt.Fprintf(w, "%-24s %d\n", "provider rows", manifest.Rows)
	fmt.Fprintf(w, "%-24s %s\n", "payout bytes", manifest.PayoutBytes)
	fmt.Fprintf(w, "%-24s %d\n", "usage chunks", len(manifest.Chunks))
	fmt.Fprintf(w, "%-24s %s\n", "manifest", usage.Ref)
	fmt.Fprintf(w, "%-24s %s\n", "written to", out)
	fmt.Fprintf(w, "%-24s %v\n", "stored in blob store", stored)
	fmt.Fprintf(w, "%-24s %s\n", "took", took.Round(time.Second))
}

// stV2RecoveryRecord is the operator's explicit command recording a scheduled
// recovery source epoch's provider rows, built with the v2 rules from its
// preserved usage artifact (--usage names the manifest; --dir reads it from
// a directory instead of the blob store). It publishes the rows' chunks,
// retains the wallet resolutions and records who made the record. Standard
// output carries counts and references only.
func stV2RecoveryRecord(opts docopt.Opts) {
	epochStr, _ := opts.String("--epoch")
	epoch, err := strconv.ParseUint(epochStr, 10, 64)
	if err != nil {
		panic(fmt.Errorf("bad --epoch %q: %s", epochStr, err))
	}
	usage, _ := opts.String("--usage")
	dir, _ := opts.String("--dir")
	by, _ := opts.String("--by")
	record, err := controller.StLedgerV2RecordRecoveryRows(context.Background(), epoch, usage, dir, by)
	if err != nil {
		panic(err)
	}
	verb := "recorded by " + by
	if record.AlreadyRecorded {
		verb = "already recorded; nothing changed"
	}
	fmt.Printf("epoch %d recovery rows %s: %d rows, %d eligible with %d coldkeys, %s of %s payout bytes eligible, chunks %s\n",
		record.Epoch, verb, record.Rows, record.EligibleRows, record.Coldkeys, record.EligibleBytes.Dec(), record.PayoutBytes.Dec(), strings.Join(record.ProviderChunks, ","))
}

// stV2Activation shows, as one JSON object, the settlement v2 activation latch
// of the deployment the loaded st.yml selects (SV-9): whether it is armed, the
// recorded pool, first epoch and v2 pair, and the latch's refusal of the loaded
// st.yml, if any. Check that it is armed before the v2 pair is ever removed
// from st.yml: until the server has read the pool's registration nothing is
// recorded, and removing the pair is not refused. `sn25golive st-check
// --v2-activation` checks a candidate st.yml against this output.
func stV2Activation(opts docopt.Opts) {
	if err := json.NewEncoder(os.Stdout).Encode(controller.GetStLedgerV2ActivationStatus(context.Background())); err != nil {
		panic(err)
	}
}

// stV2ActivationClear is the operator's explicit command removing the recorded
// settlement v2 activation of the deployment the loaded st.yml selects, for a
// record that names a wrong pool or pair (SV-9). It must name every recorded
// value exactly as `st v2-activation` shows it, and who clears it, which is
// logged. The server records the activation again the next time it reads the
// pool's registration, after a restart if it already read it. Clearing a
// correct record disarms the latch until then.
func stV2ActivationClear(opts docopt.Opts) {
	named := &model.StLedgerV2Activation{}
	for _, number := range []struct {
		option string
		value  *uint64
	}{{"--no_id", &named.NoId}, {"--first_epoch", &named.FirstEpoch}} {
		text, _ := opts.String(number.option)
		parsed, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			panic(fmt.Errorf("bad %s %q: %s", number.option, text, err))
		}
		*number.value = parsed
	}
	named.VaultV2Address, _ = opts.String("--vault_v2_address")
	named.SettlementV2Address, _ = opts.String("--settlement_v2_address")
	by, _ := opts.String("--by")
	if err := controller.StLedgerV2ClearActivation(context.Background(), named, by); err != nil {
		panic(err)
	}
	fmt.Printf("activation of pool %d from epoch %d cleared by %s\n", named.NoId, named.FirstEpoch, by)
}
