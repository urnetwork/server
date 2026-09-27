package controller

import (
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urnetwork/server/v2026/model"
)

func stringPointer(v string) *string { return &v }

func TestStReplacementFee(t *testing.T) {
	cases := []struct {
		old, suggested, want string
	}{
		{"", "100", "100"},
		{"100", "1", "113"}, // ceil(100*9/8)
		{"8", "8", "9"},
		{"100", "200", "200"},
	}
	for _, tc := range cases {
		var old *string
		if tc.old != "" {
			old = stringPointer(tc.old)
		}
		suggested, _ := new(big.Int).SetString(tc.suggested, 10)
		if got := stReplacementFee(old, suggested).String(); got != tc.want {
			t.Fatalf("old=%q suggested=%s got=%s want=%s", tc.old, tc.suggested, got, tc.want)
		}
	}
}

func TestStDecodeStoredTransactionAuthenticatesHash(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	to := common.HexToAddress("0x0000000000000000000000000000000000000123")
	tx := types.NewTx(&types.LegacyTx{Nonce: 7, To: &to, Gas: 21_000, GasPrice: big.NewInt(1), Value: big.NewInt(0)})
	tx, err = types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(945)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := tx.MarshalBinary()
	attempt := &model.StTransactionAttempt{Attempt: 1, TxHash: tx.Hash().Hex(), RawTransaction: raw}
	decoded, err := stDecodeStoredTransaction(attempt)
	if err != nil || decoded.Hash() != tx.Hash() {
		t.Fatalf("decoded=%v err=%v", decoded, err)
	}
	attempt.TxHash = common.HexToHash("0xbeef").Hex()
	if _, err := stDecodeStoredTransaction(attempt); err == nil {
		t.Fatal("stored hash mismatch accepted")
	}
}

func TestStBroadcastErrorClassification(t *testing.T) {
	for _, message := range []string{"already known", "nonce too low", "replacement transaction underpriced"} {
		if !stAmbiguousBroadcastError(errors.New(message)) {
			t.Fatalf("%q should be classified as ambiguous/possibly accepted", message)
		}
	}
	if stAmbiguousBroadcastError(errors.New("insufficient funds")) {
		t.Fatal("definitive pre-broadcast error classified as known transaction")
	}
}

// A stale-deployment cancellation can consume only the reserved nonce: its
// payload is an intrinsic-gas, zero-data self-transaction, never old calldata.
func TestStTransactionCancellationPayloadCannotTargetCoordinator(t *testing.T) {
	from := common.HexToAddress("0x1000000000000000000000000000000000000001")
	coordinator := common.HexToAddress("0x2000000000000000000000000000000000000002")
	intent := &model.StTransactionIntent{ToAddress: coordinator.Hex(), Calldata: []byte{1, 2, 3}}
	to, calldata, gasLimit, err := stTransactionAttemptPayload(intent, from, model.StTxAttemptCancellation)
	if err != nil || to != from || len(calldata) != 0 || gasLimit != 21_000 {
		t.Fatalf("cancellation payload = %s %x %d, %v", to, calldata, gasLimit, err)
	}
	executionTo, executionCalldata, executionGas, err := stTransactionAttemptPayload(intent, from, model.StTxAttemptExecution)
	if err != nil || executionTo != coordinator || string(executionCalldata) != string(intent.Calldata) || executionGas != 0 {
		t.Fatalf("execution payload = %s %x %d, %v", executionTo, executionCalldata, executionGas, err)
	}
	executionCalldata[0] = 9
	if intent.Calldata[0] != 1 {
		t.Fatal("attempt payload aliases immutable intent calldata")
	}
}
