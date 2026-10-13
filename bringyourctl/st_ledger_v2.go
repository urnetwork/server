package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/docopt/docopt-go"

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
