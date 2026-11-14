package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

const contractExpiryRepairInputLimit = 8192
const contractExpiryRepairOutputLimit = 131072
const contractExpiryRepairCommandBudget = 15 * time.Second

// The operator supplies exact IDs only through the bounded private input pipe.
// Apply is a separate explicit command flag; a request cannot enable it itself.
type privateContractExpiryRepairInput struct {
	Schema                 int      `json:"schema"`
	Kind                   string   `json:"kind"`
	ExpectedPayerNetworkId string   `json:"expected_payer_network_id"`
	ContractIds            []string `json:"contract_ids"`
}

// parsePrivateContractExpiryRepair rejects duplicate keys, trailing values,
// oversized input, noncanonical IDs and repeated contracts before invoking a model.
func parsePrivateContractExpiryRepair(reader io.Reader) (privateContractExpiryRepairInput, error) {
	return parsePrivateContractRepairKind(reader, "customer-contract-expiry-repair-v1")
}

func parsePrivateContractRepairKind(reader io.Reader, kind string) (privateContractExpiryRepairInput, error) {
	var input privateContractExpiryRepairInput
	refused := errors.New("private_expiry_input_refused")
	raw, err := io.ReadAll(io.LimitReader(reader, contractExpiryRepairInputLimit+1))
	if err != nil || len(raw) > contractExpiryRepairInputLimit {
		return input, refused
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return input, refused
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return input, refused
		}
		if _, exists := fields[key]; exists {
			return input, refused
		}
		switch key {
		case "schema", "kind", "expected_payer_network_id", "contract_ids":
		default:
			return input, refused
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return input, refused
		}
		fields[key] = value
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || len(fields) != 4 {
		return input, refused
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return input, refused
	}
	if json.Unmarshal(fields["schema"], &input.Schema) != nil || input.Schema != 1 ||
		json.Unmarshal(fields["kind"], &input.Kind) != nil || input.Kind != kind ||
		json.Unmarshal(fields["expected_payer_network_id"], &input.ExpectedPayerNetworkId) != nil ||
		json.Unmarshal(fields["contract_ids"], &input.ContractIds) != nil || len(input.ContractIds) < 1 || 32 < len(input.ContractIds) {
		return privateContractExpiryRepairInput{}, refused
	}
	validID := func(value string) bool {
		id, err := server.ParseId(value)
		return err == nil && id != (server.Id{}) && id.String() == value
	}
	if !validID(input.ExpectedPayerNetworkId) {
		return privateContractExpiryRepairInput{}, refused
	}
	seen := make(map[string]bool)
	for _, id := range input.ContractIds {
		if !validID(id) || seen[id] {
			return privateContractExpiryRepairInput{}, refused
		}
		seen[id] = true
	}
	return input, nil
}

// invokeContractExpiryRepair leaves financial decisions to the existing model
// owners; neither the command nor its private request can supply usage or money.
func invokeContractExpiryRepair(ctx context.Context, input privateContractExpiryRepairInput, apply bool) (any, error) {
	payer, err := server.ParseId(input.ExpectedPayerNetworkId)
	if err != nil {
		return nil, errors.New("private_expiry_input_refused")
	}
	ids := make([]server.Id, 0, len(input.ContractIds))
	for _, raw := range input.ContractIds {
		id, err := server.ParseId(raw)
		if err != nil {
			return nil, errors.New("private_expiry_input_refused")
		}
		ids = append(ids, id)
	}
	return model.RepairContractExpiry(ctx, model.ContractExpiryRepairRequest{
		ExpectedPayerNetworkId: payer,
		ContractIds:            ids,
		Apply:                  apply,
	})
}

// runPrivateContractExpiryRepair emits a finite private envelope. Root's launcher
// captures both streams privately; raw model errors never enter this envelope.
func runPrivateContractExpiryRepair(parent context.Context, reader io.Reader, writer io.Writer, apply bool,
	invoke func(context.Context, privateContractExpiryRepairInput, bool) (any, error),
) (exitCode int) {
	return runPrivateContractRepairKind(parent, reader, writer, apply,
		"customer-contract-expiry-repair-v1", "customer-contract-expiry-repair-private-v1", invoke)
}

func runPrivateContractRepairKind(parent context.Context, reader io.Reader, writer io.Writer, apply bool,
	inputKind, outputKind string, invoke func(context.Context, privateContractExpiryRepairInput, bool) (any, error),
) (exitCode int) {
	started := time.Now().UTC()
	status := "input_refused"
	var result any
	defer func() {
		if recover() != nil {
			status = "model_panic"
			result = nil
			exitCode = 1
		}
		body, err := json.Marshal(struct {
			Schema      int       `json:"schema"`
			Kind        string    `json:"kind"`
			Apply       bool      `json:"apply"`
			StartedAt   time.Time `json:"started_at"`
			CompletedAt time.Time `json:"completed_at"`
			Status      string    `json:"status"`
			ModelResult any       `json:"model_result"`
		}{1, outputKind, apply, started, time.Now().UTC(), status, result})
		if err != nil || len(body)+1 > contractExpiryRepairOutputLimit {
			body, _ = json.Marshal(struct {
				Schema int    `json:"schema"`
				Kind   string `json:"kind"`
				Status string `json:"status"`
			}{1, outputKind, "output_refused"})
			exitCode = 1
		}
		if _, err := writer.Write(append(body, '\n')); err != nil {
			exitCode = 1
		}
	}()
	input, err := parsePrivateContractRepairKind(reader, inputKind)
	if err != nil {
		return 1
	}
	ctx, cancel := context.WithTimeout(parent, contractExpiryRepairCommandBudget)
	defer cancel()
	if ctx.Err() != nil {
		status = "parent_canceled"
		return 1
	}
	result, err = invoke(ctx, input, apply)
	if err != nil {
		switch {
		case parent.Err() != nil:
			status = "parent_canceled"
		case errors.Is(err, context.DeadlineExceeded), ctx.Err() == context.DeadlineExceeded:
			status = "deadline"
		default:
			status = "model_error"
		}
		return 1
	}
	status = "model_returned"
	return 0
}
