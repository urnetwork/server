// Real Circle listing/token decoders must establish asset identity before a
// legacy payment can bind or release. All processor responses are synthetic.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const legacyAssetTestAddress = "0x00000000000000000000000000000000000000aB"

// The transport uses actual JSON boundaries; no proof bit is assigned by a test.
func legacyAssetTestClient(t testing.TB, transaction *CircleListedTransaction, token map[string]any, configuredAddress string) *CoreCircleApiClient {
	t.Helper()
	closed := 0
	client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
		var value any
		switch request.URL.Path {
		case "/v1/w3s/transactions":
			value = map[string]any{"data": map[string]any{"transactions": []*CircleListedTransaction{transaction}}}
		case "/v1/w3s/tokens/" + transaction.TokenId:
			value = map[string]any{"data": map[string]any{"token": token}}
		default:
			return nil, fmt.Errorf("unexpected synthetic asset request: %s", request.URL.Path)
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return paymentReadTestResponse(200, string(body), &closed), nil
	}})
	client.readUsdcAddress = func(chain string) (string, error) {
		if chain != transaction.Blockchain {
			return "", fmt.Errorf("unexpected synthetic asset chain")
		}
		return configuredAddress, nil
	}
	return client
}

// A foreign asset with the exact payout amount cannot extinguish the debt.
func TestLegacyProviderPaymentAssetRejectsForeignTransfer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, payment, key := legacyReconcileTestFixture(t)
		transfer := legacyReconcileTestTransfer("synthetic-foreign-asset", "COMPLETE", "9.990000", payment.CreateTime.Add(time.Minute))
		transfer.TxHash = "synthetic-foreign-chain-hash"
		client := legacyAssetTestClient(t, transfer, map[string]any{"id": transfer.TokenId, "blockchain": "MATIC",
			"tokenAddress": "0x00000000000000000000000000000000000000cd", "isNative": false, "symbol": "USDC"}, legacyAssetTestAddress)
		result, err := reconcileLegacyProviderPaymentAttempts(owner, &LegacyProviderPaymentReconcileSettings{}, client,
			legacyReconcileTestPayoutWalletId, func() time.Time { return server.NowUtc().Add(2 * legacyProviderPaymentMinAge) })
		if err != nil || result.AmbiguousCount != 1 || result.FoundCount != 0 || result.AbsentCount != 0 {
			t.Fatalf("foreign token became payout evidence: %+v %v", result, err)
		}
		requireLegacyAttemptHeld(t, owner, payment, key)
		if balance := model.GetAccountBalance(owner); balance.Balance.PaidByteCount != 0 {
			t.Fatal("wrong-asset observation credited paid bytes", balance)
		}
	})
}

// Verified historical USDC remains payable and credits its balance only once.
func TestLegacyProviderPaymentAssetVerifiedTransferCompletesOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, payment, _ := legacyReconcileTestFixture(t)
		transfer := legacyReconcileTestTransfer("synthetic-verified-asset", "COMPLETE", "9.990000", payment.CreateTime.Add(time.Minute))
		client := legacyAssetTestClient(t, transfer, map[string]any{"id": transfer.TokenId, "blockchain": "MATIC",
			"tokenAddress": strings.ToLower(legacyAssetTestAddress), "isNative": false}, legacyAssetTestAddress)
		result, err := reconcileLegacyProviderPaymentAttempts(owner, &LegacyProviderPaymentReconcileSettings{}, client,
			legacyReconcileTestPayoutWalletId, func() time.Time { return server.NowUtc().Add(2 * legacyProviderPaymentMinAge) })
		if err != nil || result.FoundCount != 1 || result.RefusedCount != 0 {
			t.Fatalf("verified legacy asset did not bind: %+v %v", result, err)
		}
		NewCircleClient().(*mockCircleApiClient).GetTransactionFunc = func(_ context.Context, id string) (*GetTransactionResult, error) {
			return &GetTransactionResult{Transaction: CircleTransaction{Id: id, TokenId: transfer.TokenId, Blockchain: "MATIC",
				State: "COMPLETE", TxHash: "synthetic-verified-chain-hash"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
		}
		for range 2 {
			advanced, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
			if err != nil || !advanced.Complete {
				t.Fatal("verified historical payment did not complete", advanced, err)
			}
		}
		if balance := model.GetAccountBalance(owner); balance.Balance.PaidByteCount != payment.PayoutByteCount {
			t.Fatal("verified legacy completion was not credited exactly once", balance)
		}
	})
}

// Missing identity, another chain, native assets and Solana case changes never
// become evidence, including terminal failed transfers used to prove absence.
func TestLegacyProviderPaymentAssetIdentityAndUnknownBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name, chain, configured, address, state                          string
		missingId, wrongId, wrongChain, missingNative, native, wantFound bool
	}{
		{name: "evm case", chain: "MATIC", configured: legacyAssetTestAddress, address: strings.ToLower(legacyAssetTestAddress), state: "COMPLETE", wantFound: true},
		{name: "solana exact", chain: "SOL", configured: "SyntheticMintAbC", address: "SyntheticMintAbC", state: "COMPLETE", wantFound: true},
		{name: "solana case", chain: "SOL", configured: "SyntheticMintAbC", address: "SyntheticMintabc", state: "COMPLETE"},
		{name: "foreign contract", chain: "MATIC", configured: legacyAssetTestAddress, address: "synthetic-foreign-contract", state: "COMPLETE"},
		{name: "missing id", chain: "MATIC", configured: legacyAssetTestAddress, address: legacyAssetTestAddress, state: "COMPLETE", missingId: true},
		{name: "wrong response id", chain: "MATIC", configured: legacyAssetTestAddress, address: legacyAssetTestAddress, state: "COMPLETE", wrongId: true},
		{name: "wrong response chain", chain: "MATIC", configured: legacyAssetTestAddress, address: legacyAssetTestAddress, state: "COMPLETE", wrongChain: true},
		{name: "native", chain: "MATIC", configured: legacyAssetTestAddress, address: legacyAssetTestAddress, state: "COMPLETE", native: true},
		{name: "missing native", chain: "MATIC", configured: legacyAssetTestAddress, address: legacyAssetTestAddress, state: "FAILED", missingNative: true},
		{name: "foreign failed", chain: "MATIC", configured: legacyAssetTestAddress, address: "synthetic-foreign-contract", state: "FAILED"},
		{name: "unknown failed", chain: "MATIC", configured: legacyAssetTestAddress, address: legacyAssetTestAddress, state: "FAILED", missingId: true},
	} {
		planned := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		attempt := legacyReconcileTestAttempt(model.UsdToNanoCents(5), planned)
		attempt.Blockchain = testCase.chain
		transfer := legacyReconcileTestTransfer("synthetic-asset-boundary", testCase.state, "4.990000", planned.Add(time.Minute))
		transfer.Blockchain = testCase.chain
		if testCase.missingId {
			transfer.TokenId = ""
		}
		token := map[string]any{"id": transfer.TokenId, "blockchain": testCase.chain, "tokenAddress": testCase.address, "symbol": "USDC"}
		if !testCase.missingNative {
			token["isNative"] = testCase.native
		}
		if testCase.wrongId {
			token["id"] = "synthetic-other-token"
		}
		if testCase.wrongChain {
			token["blockchain"] = "SOL"
		}
		client := legacyAssetTestClient(t, transfer, token, testCase.configured)
		listed, err := client.ListDestinationTransactions(t.Context(), testCase.chain, transfer.DestinationAddress)
		if err != nil {
			if testCase.wantFound || !testCase.wrongId && !testCase.missingNative {
				t.Errorf("%s: unexpected token read refusal: %v", testCase.name, err)
			}
			continue
		}
		decision := decideLegacyProviderPaymentGroup([]*model.LegacyProviderPaymentAttempt{attempt}, listed, nil, legacyReconcileTestWalletId)
		if testCase.wantFound {
			if len(decision.foundTransactions) != 1 || len(decision.heldPaymentIds) != 0 {
				t.Errorf("%s: exact asset was not attributable: %+v", testCase.name, decision)
			}
		} else if len(decision.heldPaymentIds) != 1 || len(decision.absentPaymentIds) != 0 || len(decision.foundTransactions) != 0 {
			t.Errorf("%s: unverified asset decided an obligation: %+v", testCase.name, decision)
		}
	}
}

// Complete listing validation still runs, but irrelevant history cannot spend
// token capacity or fail a decision through an unnecessary metadata request.
func TestLegacyProviderPaymentAssetRelevantHistoryOnly(t *testing.T) {
	for _, exclusion := range []string{"before window", "inbound", "end user", "already owned"} {
		planned := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		attempt := legacyReconcileTestAttempt(model.UsdToNanoCents(5), planned)
		relevant := legacyReconcileTestTransfer("synthetic-relevant-transfer", "COMPLETE", "4.990000", planned)
		irrelevant := legacyReconcileTestTransfer("synthetic-irrelevant-transfer", "COMPLETE", "4.990000", planned)
		irrelevant.TokenId = "synthetic-unavailable-old-token"
		bound := map[string]bool{}
		switch exclusion {
		case "before window":
			irrelevant.CreateDate = planned.Add(-legacyProviderPaymentWindowMargin - time.Second)
		case "inbound":
			irrelevant.TransactionType = "INBOUND"
		case "end user":
			irrelevant.CustodyType = "ENDUSER"
		case "already owned":
			bound[irrelevant.Id] = true
		}
		closed, tokenReads := 0, 0
		client := paymentReadTestCircle(paymentReadHooks{do: func(request *http.Request) (*http.Response, error) {
			var value any
			switch request.URL.Path {
			case "/v1/w3s/transactions":
				value = map[string]any{"data": map[string]any{"transactions": []*CircleListedTransaction{irrelevant, relevant}}}
			case "/v1/w3s/tokens/" + relevant.TokenId:
				tokenReads++
				value = map[string]any{"data": map[string]any{"token": map[string]any{
					"id": relevant.TokenId, "blockchain": "MATIC", "tokenAddress": legacyAssetTestAddress, "isNative": false}}}
			default:
				return nil, fmt.Errorf("irrelevant history requested unavailable metadata: %s", request.URL.Path)
			}
			body, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			return paymentReadTestResponse(200, string(body), &closed), nil
		}})
		client.readUsdcAddress = func(string) (string, error) { return legacyAssetTestAddress, nil }
		attempts := []*model.LegacyProviderPaymentAttempt{attempt}
		listed, err := listLegacyProviderPaymentTransactions(t.Context(), client, "MATIC", relevant.DestinationAddress, attempts, bound)
		if err != nil || len(listed) != 2 || tokenReads != 1 {
			t.Errorf("%s: irrelevant asset blocked a complete listing: %d reads, %v", exclusion, tokenReads, err)
			continue
		}
		decision := decideLegacyProviderPaymentGroup(attempts, listed, bound, legacyReconcileTestWalletId)
		if len(decision.foundTransactions) != 1 || len(decision.heldPaymentIds) != 0 {
			t.Errorf("%s: relevant verified transfer was not found: %+v", exclusion, decision)
		}
	}
}
