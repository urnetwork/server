// Synthetic controls exercise the new command boundary without invoking a
// model, reading configuration or contacting a database.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const syntheticRedisExpiryPayer = "10000000-0000-4000-8000-000000000001"
const syntheticRedisExpiryContract = "20000000-0000-4000-8000-000000000001"

// The requested count creates distinct, visibly synthetic contract identities.
func syntheticRedisExpiryRepairInput(contractCount int) string {
	contractJsons := make([]string, 0, contractCount)
	for index := 0; index < contractCount; index++ {
		contractJsons = append(contractJsons, fmt.Sprintf(`"20000000-0000-4000-8000-%012x"`, index+1))
	}
	return `{"schema":1,"kind":"` + privateRedisContractExpiryRepairInputKind + `","expected_payer_network_id":"` + syntheticRedisExpiryPayer + `","contract_ids":[` + strings.Join(contractJsons, ",") + `]}`
}

// Redis expiry and the existing expiry command must not accept each other's kind.
func TestPrivateRedisContractExpiryRepairKindIsolation(t *testing.T) {
	raw := syntheticRedisExpiryRepairInput(1)
	input, err := parsePrivateRedisContractExpiryRepair(strings.NewReader(raw))
	if err != nil || input.Kind != privateRedisContractExpiryRepairInputKind || input.ExpectedPayerNetworkId != syntheticRedisExpiryPayer || len(input.ContractIds) != 1 || input.ContractIds[0] != syntheticRedisExpiryContract {
		t.Fatal("the Redis kind lost its exact synthetic scope")
	}
	if _, err := parsePrivateContractExpiryRepair(strings.NewReader(raw)); err == nil {
		t.Fatal("the existing expiry parser admitted the Redis kind")
	}
	for _, otherKind := range []string{"customer-contract-expiry-repair-v1", privateLegacySettlementDrainInputKind} {
		otherRaw := strings.Replace(raw, privateRedisContractExpiryRepairInputKind, otherKind, 1)
		if _, err := parsePrivateRedisContractExpiryRepair(strings.NewReader(otherRaw)); err == nil {
			t.Fatal("the Redis parser admitted another command kind")
		}
	}
}

// Strict input refusal precedes every model invocation, including explicit apply.
func TestPrivateRedisContractExpiryRepairRejectsInputBeforeModel(t *testing.T) {
	raw := syntheticRedisExpiryRepairInput(1)
	cases := []struct {
		name string
		raw  string
	}{
		{name: "other-kind", raw: strings.Replace(raw, privateRedisContractExpiryRepairInputKind, "customer-contract-expiry-repair-v1", 1)},
		{name: "duplicate-field", raw: strings.Replace(raw, `"schema":1`, `"schema":1,"schema":1`, 1)},
		{name: "usage-input", raw: strings.Replace(raw, `"schema":1`, `"schema":1,"used_transfer_byte_count":0`, 1)},
		{name: "amount-input", raw: strings.Replace(raw, `"schema":1`, `"schema":1,"amount_usd":0`, 1)},
		{name: "apply-input", raw: strings.Replace(raw, `"schema":1`, `"schema":1,"apply":true`, 1)},
		{name: "trailing-value", raw: raw + `{}`},
		{name: "repeated-contract", raw: strings.Replace(raw, `"`+syntheticRedisExpiryContract+`"`, `"`+syntheticRedisExpiryContract+`","`+syntheticRedisExpiryContract+`"`, 1)},
		{name: "empty-scope", raw: syntheticRedisExpiryRepairInput(0)},
		{name: "over-scope-cap", raw: syntheticRedisExpiryRepairInput(33)},
	}
	for _, testCase := range cases {
		var output bytes.Buffer
		called := false
		code := runPrivateRedisContractExpiryRepair(context.Background(), strings.NewReader(testCase.raw), &output, true,
			func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
				called = true
				return nil, nil
			})
		var envelope map[string]any
		if code != 1 || called || json.Unmarshal(output.Bytes(), &envelope) != nil || envelope["kind"] != privateRedisContractExpiryRepairOutputKind || envelope["status"] != "input_refused" || envelope["model_result"] != nil || strings.Contains(output.String(), syntheticRedisExpiryPayer) || strings.Contains(output.String(), syntheticRedisExpiryContract) {
			t.Fatalf("case %s reached the model or escaped finite private refusal", testCase.name)
		}
	}
}

// Both explicit modes preserve the exact scope and the shared command budget.
func TestPrivateRedisContractExpiryRepairModeAndDeadline(t *testing.T) {
	for _, apply := range []bool{false, true} {
		var output bytes.Buffer
		calls := 0
		code := runPrivateRedisContractExpiryRepair(context.Background(), strings.NewReader(syntheticRedisExpiryRepairInput(32)), &output, apply,
			func(ctx context.Context, input privateContractExpiryRepairInput, gotApply bool) (any, error) {
				calls++
				deadline, ok := ctx.Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining <= 0 || remaining > contractExpiryRepairCommandBudget || gotApply != apply || input.Kind != privateRedisContractExpiryRepairInputKind || input.ExpectedPayerNetworkId != syntheticRedisExpiryPayer || len(input.ContractIds) != 32 || input.ContractIds[0] != syntheticRedisExpiryContract || input.ContractIds[31] != "20000000-0000-4000-8000-000000000020" {
					t.Fatal("the command lost its mode, bounded scope or deadline")
				}
				return map[string]bool{"synthetic_result": true}, nil
			})
		var envelope map[string]any
		if code != 0 || calls != 1 || json.Unmarshal(output.Bytes(), &envelope) != nil || envelope["kind"] != privateRedisContractExpiryRepairOutputKind || envelope["status"] != "model_returned" || envelope["apply"] != apply {
			t.Fatal("the command did not preserve its private result and explicit mode")
		}
	}
}

// Cancellation prevents invocation or propagates to it; errors and panics never
// echo private input or raw failure text into the finite envelope.
func TestPrivateRedisContractExpiryRepairCancellationAndPrivacy(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		wantCalled bool
	}{
		{name: "pre-canceled", status: "parent_canceled", wantCalled: false},
		{name: "in-flight-canceled", status: "parent_canceled", wantCalled: true},
		{name: "model-error", status: "model_error", wantCalled: true},
		{name: "model-panic", status: "model_panic", wantCalled: true},
		{name: "deadline", status: "deadline", wantCalled: true},
	}
	for _, testCase := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		if testCase.name == "pre-canceled" {
			cancel()
		}
		var output bytes.Buffer
		called := false
		code := runPrivateRedisContractExpiryRepair(ctx, strings.NewReader(syntheticRedisExpiryRepairInput(1)), &output, true,
			func(invokeCtx context.Context, input privateContractExpiryRepairInput, apply bool) (any, error) {
				called = true
				switch testCase.name {
				case "in-flight-canceled":
					cancel()
					if invokeCtx.Err() != context.Canceled {
						t.Fatal("parent cancellation did not reach the model context")
					}
					return nil, invokeCtx.Err()
				case "model-panic":
					panic("synthetic-private-redis-error " + input.ExpectedPayerNetworkId)
				case "deadline":
					return nil, context.DeadlineExceeded
				default:
					return nil, errors.New("synthetic-private-redis-error " + input.ExpectedPayerNetworkId)
				}
			})
		cancel()
		var envelope map[string]any
		if code != 1 || called != testCase.wantCalled || output.Len() > contractExpiryRepairOutputLimit || json.Unmarshal(output.Bytes(), &envelope) != nil || envelope["kind"] != privateRedisContractExpiryRepairOutputKind || envelope["status"] != testCase.status || envelope["model_result"] != nil || strings.Contains(output.String(), "synthetic-private-redis-error") || strings.Contains(output.String(), syntheticRedisExpiryPayer) || strings.Contains(output.String(), syntheticRedisExpiryContract) {
			t.Fatalf("case %s escaped cancellation or finite private failure", testCase.name)
		}
	}
}

// Oversized model output retains the Redis output kind in the shared refusal.
func TestPrivateRedisContractExpiryRepairOutputCap(t *testing.T) {
	var output bytes.Buffer
	code := runPrivateRedisContractExpiryRepair(context.Background(), strings.NewReader(syntheticRedisExpiryRepairInput(1)), &output, false,
		func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
			return strings.Repeat("x", contractExpiryRepairOutputLimit+1), nil
		})
	var envelope map[string]any
	if code != 1 || output.Len() > contractExpiryRepairOutputLimit || json.Unmarshal(output.Bytes(), &envelope) != nil || len(envelope) != 3 || envelope["kind"] != privateRedisContractExpiryRepairOutputKind || envelope["status"] != "output_refused" {
		t.Fatal("oversized output escaped the Redis-specific finite refusal")
	}
}
