package controller

// The mainnet dry run of a 493 spend, before the first real charge: a spend
// of one gamma at our service, signed by a throwaway sr25519 key made here
// and never stored, judged by the chain at its finalized head with
// TaggedTransactionQueue_validate_transaction. Nothing is submitted, and
// nothing is signed with any key of ours. The key has no deposit, so the
// expected answer is the transaction extension's refusal of the spend,
// invalid/custom/255 (BadRequest): the chain decoded the call, accepted its
// signature, era and nonce, and admitted a fee-free call from an account
// with no providers, up to the deposit check. Any other verdict stops the
// ship.
//
// It runs against Finney only with GAMMA_DRY_RUN_RPC set (a JSON-RPC url);
// GAMMA_DRY_RUN_SERVICE names the service hotkey (default: SN25's owner
// hotkey, the service gamma.yml names):
//
//	GAMMA_DRY_RUN_RPC=https://archive.chain.opentensor.ai go test ./controller/ -run '^TestGammaMainnetDryRun$' -v -count=1

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/signature"

	"github.com/urnetwork/server/model"
)

const gammaDryRunDefaultService = "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG"

func TestGammaMainnetDryRun(t *testing.T) {
	url := os.Getenv("GAMMA_DRY_RUN_RPC")
	if url == "" {
		t.Skip("GAMMA_DRY_RUN_RPC is not set")
	}
	service := os.Getenv("GAMMA_DRY_RUN_SERVICE")
	if service == "" {
		service = gammaDryRunDefaultService
	}
	verdict := gammaDryRun(t, newGammaChain(newGammaHttpRpc([]string{url})), service, gammaDefaultNetuid, nil)
	if verdict != "invalid/custom/255" {
		t.Fatalf("a spend from a key with no deposit is %s on the chain, not invalid/custom/255: stop and read why before shipping", verdict)
	}
}

// The dry run against the simulated chain, which judges a spend as runtime
// 493 does: a key with no deposit is refused, and one with a deposit that
// covers the spend is valid.
func TestGammaDryRunAgainstTheSim(t *testing.T) {
	settings := gammaTestSettings(t)
	sim := newGammaSimChain(t, settings)
	chain := newGammaChain(sim)
	connectAssertVerdict(t, gammaDryRun(t, chain, gammaAddress(settings.ServiceHotkey), settings.Netuid, nil), "invalid/custom/255")
	// the same spend from a key whose deposit covers it is valid
	withDeposit := func(signer [32]byte) {
		sim.setDeposit(signer, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), balance: 5 * gammaUnitsPerGamma})
	}
	connectAssertVerdict(t, gammaDryRun(t, chain, gammaAddress(settings.ServiceHotkey), settings.Netuid, withDeposit), "valid")
}

func connectAssertVerdict(t testing.TB, verdict string, want string) {
	t.Helper()
	if verdict != want {
		t.Fatalf("verdict %s, want %s", verdict, want)
	}
}

// gammaDryRun signs a spend of one gamma at the service with a new throwaway
// key, and returns the chain's verdict on it at the finalized head ("valid",
// or gammaValidity's Verdict). prepare, when given, sees the key first.
func gammaDryRun(t testing.TB, chain *gammaChain, service string, netuid uint16, prepare func(signer [32]byte)) string {
	ctx := context.Background()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	signer, err := signature.KeyringPairFromSecret("0x"+hex.EncodeToString(seed), model.BittensorSS58Prefix)
	if err != nil {
		t.Fatal(err)
	}
	var signerKey [32]byte
	copy(signerKey[:], signer.PublicKey)
	serviceKey, err := model.DecodeBittensorAddress(service)
	if err != nil {
		t.Fatalf("service %s: %v", service, err)
	}
	head, number, err := chain.FinalizedHead(ctx)
	if err != nil {
		t.Fatalf("finalized head: %v", err)
	}
	runtime, err := chain.RuntimeAt(ctx, head)
	if err != nil {
		t.Fatalf("runtime at %d: %v", number, err)
	}
	if err := runtime.Supported(); err != nil {
		t.Fatalf("runtime %d at block %d is not one the biller supports: %v", runtime.SpecVersion, number, err)
	}
	genesis, err := chain.GenesisHash(ctx)
	if err != nil {
		t.Fatalf("genesis: %v", err)
	}
	account, err := chain.Account(ctx, runtime, head, signerKey)
	if err != nil {
		t.Fatalf("the throwaway key's account: %v", err)
	}
	if prepare != nil {
		prepare(signerKey)
	}
	call, err := gammaSpendCall(runtime, serviceKey, netuid, gammaUnitsPerGamma)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := gammaSignSpend(runtime, genesis, number, head, gammaDefaultEraPeriod, account.Nonce, call, signer)
	if err != nil {
		t.Fatal(err)
	}
	validity, err := chain.ValidateTransaction(ctx, signed.Encoded, head)
	if err != nil {
		t.Fatalf("validate_transaction: %v", err)
	}
	verdict := validity.Verdict
	if validity.Valid {
		verdict = "valid"
	}
	t.Logf("runtime %d, finalized block %d: a spend of 1 gamma at %s on netuid %d, signed by a throwaway key (%s, nonce %d, %d-block era, %d bytes), is %s",
		runtime.SpecVersion, number, service, netuid, gammaAddress(signerKey), account.Nonce, gammaDefaultEraPeriod, len(signed.Encoded), verdict)
	return verdict
}
