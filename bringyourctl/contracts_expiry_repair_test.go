package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const syntheticRepairPayer = "10000000-0000-4000-8000-000000000001"
const syntheticRepairContract = "20000000-0000-4000-8000-000000000001"

func syntheticRepairInput() string {
	return `{"schema":1,"kind":"customer-contract-expiry-repair-v1","expected_payer_network_id":"` + syntheticRepairPayer + `","contract_ids":["` + syntheticRepairContract + `"]}`
}

func TestPrivateContractExpiryRepairRejectsInputBeforeModel(t *testing.T) {
	good := syntheticRepairInput()
	cases := []string{
		"", good + `{}`, strings.Repeat("x", contractExpiryRepairInputLimit+1),
		strings.Replace(good, `"schema":1`, `"schema":1,"schema":1`, 1),
		strings.Replace(good, `"schema":1`, `"schema":true`, 1),
		strings.Replace(good, `"schema":1`, `"schema":1,"apply":true`, 1),
		strings.Replace(good, `[`+`"`+syntheticRepairContract+`"`+`]`, `[]`, 1),
		strings.Replace(good, `[`+`"`+syntheticRepairContract+`"`+`]`, `null`, 1),
		strings.Replace(good, syntheticRepairPayer, "00000000-0000-0000-0000-000000000000", 1),
		strings.Replace(good, syntheticRepairContract, "not-an-id", 1),
		strings.Replace(good, `"`+syntheticRepairContract+`"`, `"`+syntheticRepairContract+`","`+syntheticRepairContract+`"`, 1),
	}
	for index, raw := range cases {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			var output bytes.Buffer
			called := false
			code := runPrivateContractExpiryRepair(context.Background(), strings.NewReader(raw), &output, true,
				func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
					called = true
					return nil, nil
				})
			if code != 1 || called || strings.Contains(output.String(), syntheticRepairPayer) || strings.Contains(output.String(), syntheticRepairContract) {
				t.Fatal("invalid private input reached the model or escaped refusal")
			}
		})
	}
}

func TestPrivateContractExpiryRepairModeAndDeadline(t *testing.T) {
	for _, apply := range []bool{false, true} {
		var output bytes.Buffer
		calls := 0
		code := runPrivateContractExpiryRepair(context.Background(), strings.NewReader(syntheticRepairInput()), &output, apply,
			func(ctx context.Context, input privateContractExpiryRepairInput, gotApply bool) (any, error) {
				calls++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > contractExpiryRepairCommandBudget || time.Until(deadline) <= 0 || gotApply != apply || input.ExpectedPayerNetworkId != syntheticRepairPayer || len(input.ContractIds) != 1 {
					t.Fatal("command lost exact scope, explicit mode or deadline")
				}
				return map[string]any{"contracts": []any{}}, nil
			})
		var envelope map[string]any
		if code != 0 || calls != 1 || json.Unmarshal(output.Bytes(), &envelope) != nil || envelope["status"] != "model_returned" || envelope["apply"] != apply {
			t.Fatal("valid command did not preserve private model return")
		}
	}
}

func TestPrivateContractExpiryRepairCancellationAndErrorsAreClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	called := false
	code := runPrivateContractExpiryRepair(ctx, strings.NewReader(syntheticRepairInput()), &output, true,
		func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
			called = true
			return nil, nil
		})
	if code != 1 || called || !strings.Contains(output.String(), `"status":"parent_canceled"`) {
		t.Fatal("canceled parent reached mutation")
	}
	for _, panicError := range []bool{false, true} {
		output.Reset()
		code := runPrivateContractExpiryRepair(context.Background(), strings.NewReader(syntheticRepairInput()), &output, true,
			func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
				if panicError {
					panic("synthetic-private-error")
				}
				return map[string]bool{"partial_prefix": true}, errors.New("synthetic-private-error")
			})
		if code != 1 || strings.Contains(output.String(), "synthetic-private-error") || strings.Contains(output.String(), syntheticRepairPayer) {
			t.Fatal("raw model error escaped finite envelope")
		}
	}
}

func TestPrivateContractExpiryRepairOutputIsBounded(t *testing.T) {
	var output bytes.Buffer
	code := runPrivateContractExpiryRepair(context.Background(), strings.NewReader(syntheticRepairInput()), &output, false,
		func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
			return strings.Repeat("x", contractExpiryRepairOutputLimit+1), nil
		})
	if code != 1 || output.Len() > contractExpiryRepairOutputLimit || !strings.Contains(output.String(), `"status":"output_refused"`) {
		t.Fatal("oversized model result was not bounded")
	}
}
