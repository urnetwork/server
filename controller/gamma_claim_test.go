package controller

// Hermetic tests of the claim flow's rules (gamma_claim.go) and of the new
// deposit's daily spend cap (gamma_controller.go): no network, no database.

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	subkey "github.com/vedhavyas/go-subkey/v2"
	"github.com/vedhavyas/go-subkey/v2/sr25519"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// gammaTestPayer is a hotkey that pays our service from its own deposit: the
// //Alice mini-secret hard-derived (public test data).
func gammaTestPayer(t testing.TB) (subkey.KeyPair, string) {
	keypair, err := subkey.DeriveKeyPair(sr25519.Scheme{}, gammaTestAliceSeed+"//gamma-direct-payer")
	if err != nil {
		t.Fatal(err)
	}
	return keypair, subkey.SS58Encode(keypair.Public(), model.BittensorSS58Prefix)
}

func gammaTestSign(t testing.TB, keypair subkey.KeyPair, message string) string {
	signature, err := keypair.Sign([]byte(message))
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(signature)
}

// The challenge is one line, naming the network, the hotkey, our service and
// when it was issued; only that exact form reads back.
func TestGammaClaimMessageRoundTrip(t *testing.T) {
	networkId := server.NewId()
	_, payer := gammaTestPayer(t)
	service := "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG"
	issued := time.Date(2026, 11, 20, 22, 30, 0, 0, time.UTC)
	message := gammaClaimMessage(networkId, payer, service, issued)
	connect.AssertEqual(t, strings.Contains(message, "\n"), false)
	connect.AssertEqual(t, message, "URnetwork Gamma claim v1: network "+networkId.String()+" hotkey "+payer+" service "+service+" issued 1795213800")
	challenge, ok := gammaParseClaimMessage(message)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, *challenge, gammaClaimChallenge{NetworkId: networkId, Hotkey: payer, Service: service, Issued: issued})

	for _, bad := range []string{
		"",
		message + "\n",
		" " + message,
		strings.Replace(message, "v1:", "v2:", 1),
		strings.Replace(message, " issued ", " issued  ", 1),
		strings.Replace(message, "1795213800", "-1", 1),
		strings.Replace(message, "1795213800", "x", 1),
		strings.Replace(message, networkId.String(), "not-an-id", 1),
		strings.Replace(message, " service ", " servce ", 1),
		message + " extra",
		strings.Replace(message, " hotkey "+payer, "", 1),
	} {
		if _, ok := gammaParseClaimMessage(bad); ok {
			t.Fatalf("read %q", bad)
		}
	}
	// btcli 11.3.5 signs it as one argument
	connect.AssertEqual(t, gammaClaimSignCommand(message), "btcli wallet sign --use-hotkey --message \""+message+"\" -w <your_wallet> -H <your_hotkey>")
}

// The payer's hotkey signature of the challenge verifies, as btcli prints it
// (bare hex) and as wallets that wrap it in <Bytes> sign it; another message's
// or another key's does not.
func TestGammaClaimSignatureVerifies(t *testing.T) {
	keypair, payer := gammaTestPayer(t)
	message := gammaClaimMessage(server.NewId(), payer, "5CyQFykVpfa9xgZVGgBsiNUpjbo1EihBTSkPvDC1FZbL1wxG", time.Unix(1795213800, 0))
	for _, signature := range []string{gammaTestSign(t, keypair, message), "0x" + gammaTestSign(t, keypair, "<Bytes>"+message+"</Bytes>")} {
		ok, err := model.VerifyBittensorSignature(payer, message, signature)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ok, true)
	}
	ok, _ := model.VerifyBittensorSignature(payer, message+" ", gammaTestSign(t, keypair, message))
	connect.AssertEqual(t, ok, false)
	other, err := subkey.DeriveKeyPair(sr25519.Scheme{}, gammaTestAliceSeed+"//someone-else")
	if err != nil {
		t.Fatal(err)
	}
	ok, _ = model.VerifyBittensorSignature(payer, message, gammaTestSign(t, other, message))
	connect.AssertEqual(t, ok, false)
}

// A claimed amount buys data at the 1 TiB pack's gamma price, rounded down to
// a whole MiB.
func TestGammaClaimDataBytesAt(t *testing.T) {
	price := int64(3 * gammaUnitsPerGamma)
	connect.AssertEqual(t, gammaClaimDataBytesAt(price, price), model.Tib)
	connect.AssertEqual(t, gammaClaimDataBytesAt(gammaUnitsPerGamma, price), model.ByteCount(349_525)*model.Mib)
	connect.AssertEqual(t, gammaClaimDataBytesAt(5*gammaUnitsPerGamma, price), model.ByteCount(1_747_626)*model.Mib)
	connect.AssertEqual(t, gammaClaimDataBytesAt(1, price), model.ByteCount(0))
	connect.AssertEqual(t, gammaClaimDataBytesAt(0, price), model.ByteCount(0))
	connect.AssertEqual(t, gammaClaimDataBytesAt(price, 0), model.ByteCount(0))
	// a claim's data item carries its byte count
	item := model.GammaClaimDataItem(model.Tib)
	connect.AssertEqual(t, len(item) <= 32, true)
	byteCount, ok := model.ParseGammaClaimDataItem(item)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, byteCount, model.Tib)
	for _, bad := range []string{"data_1tib", "claim_data_", "claim_data_-5", "claim_data_x", "monthly"} {
		if _, ok := model.ParseGammaClaimDataItem(bad); ok {
			t.Fatalf("read %q", bad)
		}
	}
}

// A new deposit's daily spend cap is the largest charge plus a quarter,
// rounded up to a whole gamma.
func TestGammaDailyCapOf(t *testing.T) {
	g := int64(gammaUnitsPerGamma)
	for _, test := range []struct {
		largest int64
		want    int64
	}{
		// a year at 40 (the largest standard charge)
		{40 * g, 50 * g},
		// one gift code, at 39.99
		{39_990_000_000, 50 * g},
		// three gift codes at 29.99
		{89_970_000_000, 113 * g},
		// the largest gift order, 20 codes at 23.99
		{479_800_000_000, 600 * g},
		// a 1 TiB pack alone
		{3 * g, 4 * g},
		{0, 1 * g},
	} {
		connect.AssertEqual(t, gammaDailyCapOf(test.largest), test.want)
	}
}

// A charge waits for the cap to refill linearly over 7200 blocks of 12
// seconds: from 10 of a 50 cap to 40 takes 4320 blocks, 14.4 hours.
func TestGammaCapRefillWait(t *testing.T) {
	g := int64(gammaUnitsPerGamma)
	wait := gammaCapRefillWait(50*g, 10*g, 40*g)
	connect.AssertEqual(t, wait, 4320*12*time.Second)
	connect.AssertEqual(t, gammaWaitText(wait), "about 15 hours")
	connect.AssertEqual(t, gammaCapRefillWait(50*g, 40*g, 40*g), time.Duration(0))
	connect.AssertEqual(t, gammaCapRefillWait(0, 0, 40*g), time.Duration(0))
	connect.AssertEqual(t, gammaWaitText(30*time.Minute), "30 minutes")
	connect.AssertEqual(t, gammaWaitText(50*time.Second), "1 minute")
	connect.AssertEqual(t, gammaWaitText(61*time.Minute), "about 2 hours")
}
