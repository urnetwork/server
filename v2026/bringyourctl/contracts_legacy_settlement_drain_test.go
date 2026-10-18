package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func syntheticLegacyDrainInput() string {
	return strings.Replace(syntheticRepairInput(), "customer-contract-expiry-repair-v1", privateLegacySettlementDrainInputKind, 1)
}

func TestPrivateLegacySettlementDrainSeparatesKindsAndScope(t *testing.T) {
	valid := syntheticLegacyDrainInput()
	ids := make([]string, 33)
	for i := range ids {
		ids[i] = server.NewId().String()
	}
	oversized := map[string]any{"schema": 1, "kind": privateLegacySettlementDrainInputKind, "expected_payer_network_id": syntheticRepairPayer, "contract_ids": ids}
	tooMany, _ := json.Marshal(oversized)
	for _, raw := range []string{syntheticRepairInput(), valid + `{}`, string(tooMany), strings.Replace(valid, `"schema":1`, `"schema":1,"schema":1`, 1)} {
		var output bytes.Buffer
		called := false
		code := runPrivateLegacySettlementDrain(context.Background(), strings.NewReader(raw), &output, true, func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
			called = true
			return nil, nil
		})
		if code != 1 || called || strings.Contains(output.String(), syntheticRepairPayer) {
			t.Fatal("wrong command kind or invalid bounded scope reached drain")
		}
	}
	if _, err := parsePrivateContractExpiryRepair(strings.NewReader(valid)); err == nil {
		t.Fatal("legacy drain request was accepted by expiry command")
	}
	for _, apply := range []bool{false, true} {
		var output bytes.Buffer
		code := runPrivateLegacySettlementDrain(context.Background(), strings.NewReader(valid), &output, apply, func(ctx context.Context, input privateContractExpiryRepairInput, gotApply bool) (any, error) {
			if input.Kind != privateLegacySettlementDrainInputKind || gotApply != apply {
				t.Fatal("drain lost explicit command kind or mode")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("drain caller is unbounded")
			}
			return map[string]any{"financial_commit_acknowledged": true, "post_processing": "returned_unverified", "mirror": "not_verified"}, nil
		})
		if code != 0 || !strings.Contains(output.String(), privateLegacySettlementDrainOutputKind) || !strings.Contains(output.String(), `"mirror":"not_verified"`) {
			t.Fatal("drain envelope lost commit versus mirror qualifiers")
		}
	}
}

func TestPrivateLegacySettlementDrainFailurePrivacy(t *testing.T) {
	for _, panicFailure := range []bool{false, true} {
		var output bytes.Buffer
		code := runPrivateLegacySettlementDrain(context.Background(), strings.NewReader(syntheticLegacyDrainInput()), &output, true, func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
			if panicFailure {
				panic("synthetic-private-ledger-error")
			}
			return nil, errors.New("synthetic-private-ledger-error")
		})
		if code != 1 || strings.Contains(output.String(), "synthetic-private-ledger-error") || strings.Contains(output.String(), syntheticRepairPayer) {
			t.Fatal("raw drain failure escaped private finite envelope")
		}
	}
	var output bytes.Buffer
	code := runPrivateLegacySettlementDrain(context.Background(), strings.NewReader(syntheticLegacyDrainInput()), &output, false, func(context.Context, privateContractExpiryRepairInput, bool) (any, error) {
		return strings.Repeat("x", contractExpiryRepairOutputLimit+1), nil
	})
	if code != 1 || output.Len() > contractExpiryRepairOutputLimit || !strings.Contains(output.String(), privateLegacySettlementDrainOutputKind) || !strings.Contains(output.String(), "output_refused") {
		t.Fatal("oversized drain envelope lost its bound or command identity")
	}
}
