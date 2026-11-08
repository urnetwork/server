// Runtime bootstrap counts only compiled synthetic text. Declared context limits
// are not observed evidence, and these results can never admit generation.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
)

const winnerMultipassRuntimeProbeKind = "winner-review-multipass-runtime-probe/v1"

type winnerMultipassRuntimeMetadata struct {
	Path        string `json:"path"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        []byte `json:"body"`
}

// Requested checkpoint pins are explicit trusted declarations, not observations
// of deployed weights. Actual context and rendering evidence comes from originals.
type winnerMultipassRuntimeObservation struct {
	Schema                       int                              `json:"schema"`
	Kind                         string                           `json:"kind"`
	Member                       winnerSnowMember                 `json:"requested_member"`
	CheckpointEvidenceKind       string                           `json:"checkpoint_evidence_kind"`
	DeclaredMaximumContextTokens int                              `json:"declared_maximum_context_tokens"`
	ObservedMaximumContextTokens int                              `json:"observed_maximum_context_tokens"`
	InputTokens                  int                              `json:"input_tokens"`
	TokenizerContractSha256      string                           `json:"tokenizer_contract_sha256"`
	RenderedPromptSha256         string                           `json:"rendered_prompt_sha256"`
	TokenizerRequestSha256       string                           `json:"tokenizer_request_sha256"`
	TokenizerResponseSha256      string                           `json:"tokenizer_response_sha256"`
	Metadata                     []winnerMultipassRuntimeMetadata `json:"original_non_inference_metadata"`
}

// The host owns a separate immutable two-probe owner and permanent exclusive
// per-member intents. This pure builder neither claims nor executes either probe.
func buildWinnerMultipassRuntimeProbe(owner winnerMultipassOwner, member winnerSnowMember, deadline time.Time) (winnerMultipassBrokerEnvelope, error) {
	provider := defaultWinnerSnowProviderSettings()
	if !slices.Contains(provider.Members, member) || owner.Id == "" || len(owner.Id) > 128 || owner.StartedAt.IsZero() || !owner.Deadline.After(owner.StartedAt) || owner.Deadline.Sub(owner.StartedAt) > 10*time.Minute || !deadline.After(owner.StartedAt) || deadline.After(owner.Deadline) {
		return winnerMultipassBrokerEnvelope{}, errors.New("runtime probe lacks its fixed synthetic member or bounded prospective owner")
	}
	settings := defaultWinnerMultipassSettings()
	for index, fixed := range provider.Members {
		settings.TokenizationProfiles[fixed.Name] = winnerMultipassTokenizationProfile{Member: fixed, MaximumContextTokens: []int{98304, 131072}[index], TokenizerContractSha256: winnerMultipassTokenizerContractSha256(), RuntimeEvidenceSha256: ""}
	}
	payload, err := json.Marshal(map[string]any{
		"model": member.Model, "reasoning": map[string]string{"effort": member.Reasoning}, "include_reasoning": false, "store": false, "stream": true, "tools": []any{}, "tool_choice": "none", "truncation": "disabled", "max_output_tokens": settings.MaximumOutputTokens,
		"instructions": "This fixed synthetic runtime probe is tokenized only. No model response or candidate review is requested.",
		"input":        "Synthetic complete-coverage tokenizer canary. Preserve every input token; report the deployed tokenizer count and maximum context through the non-inference API only.",
		"text":         map[string]any{"format": map[string]any{"type": "json_schema", "name": "competition_complete_coverage_review", "strict": true, "schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"synthetic": map[string]string{"type": "string"}}, "required": []string{"synthetic"}}}},
	})
	if err != nil {
		return winnerMultipassBrokerEnvelope{}, err
	}
	declaration, _ := json.Marshal(settings.TokenizationProfiles)
	return winnerMultipassBrokerEnvelope{Schema: 1, Kind: winnerMultipassRuntimeProbeKind, Operation: "tokenize", Owner: owner, CallDeadline: deadline, PlanSha256: winnerMultipassSha256(declaration), BundleSha256: winnerMultipassSha256(payload),
		Call: winnerMultipassCall{Id: member.Name + "-runtime-probe", Member: member, Stage: "scan", UnitIds: []string{}, Parents: []string{}}, Payload: payload, Settings: settings, AllowModelLoad: true}, nil
}

func validateWinnerMultipassRuntimeProbeEnvelope(envelope winnerMultipassBrokerEnvelope, now time.Time, limits winnerMultipassProviderLimits) error {
	expected, err := buildWinnerMultipassRuntimeProbe(envelope.Owner, envelope.Call.Member, envelope.CallDeadline)
	if err != nil || !reflect.DeepEqual(expected, envelope) || now.Before(envelope.Owner.StartedAt) || !now.Before(envelope.Owner.Deadline) || !envelope.CallDeadline.After(now) || envelope.CallDeadline.Sub(now) > limits.TokenizerTimeout {
		return errors.New("runtime probe changed fixed synthetic input, empty observation, operation or bounded clock")
	}
	return nil
}

func winnerMultipassRuntimeReady(content []byte, member winnerSnowMember) bool {
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	return json.Unmarshal(content, &running) == nil && len(running.Running) == 1 && running.Running[0].Model == member.Model && running.Running[0].State == "ready"
}

// Explicit non-inference version routing may load the fixed member. Exact
// before/version/after bytes remain visible in the contained completion journal.
func observeWinnerMultipassRuntime(ctx context.Context, client *http.Client, envelope winnerMultipassBrokerEnvelope, provider winnerSnowProviderSettings, limits winnerMultipassProviderLimits) (*winnerMultipassRuntimeObservation, error) {
	observation := &winnerMultipassRuntimeObservation{Schema: 1, Kind: "winner-review-multipass-runtime-observation/v1", Member: envelope.Call.Member, CheckpointEvidenceKind: "trusted_deployment_declaration_not_weight_attestation", DeclaredMaximumContextTokens: envelope.Settings.TokenizationProfiles[envelope.Call.Member.Name].MaximumContextTokens, TokenizerContractSha256: winnerMultipassTokenizerContractSha256(), Metadata: []winnerMultipassRuntimeMetadata{}}
	for _, path := range []string{"/running", "/upstream/" + envelope.Call.Member.Model + "/version", "/running"} {
		content, status, contentType, err := winnerMultipassHttp(ctx, client, provider, envelope.Call.Member, http.MethodGet, path, nil, limits.MaximumMetadataBytes, limits.TokenizerTimeout)
		observation.Metadata = append(observation.Metadata, winnerMultipassRuntimeMetadata{Path: path, Status: status, ContentType: contentType, Body: content})
		if err != nil || status != http.StatusOK {
			return observation, errors.New("runtime probe metadata failed; retain originals without retry")
		}
	}
	if err := validateWinnerMultipassRuntimeMetadata(envelope, observation); err != nil {
		return observation, err
	}
	return observation, nil
}

func validateWinnerMultipassRuntimeMetadata(envelope winnerMultipassBrokerEnvelope, observation *winnerMultipassRuntimeObservation) error {
	if observation == nil || len(observation.Metadata) != 3 {
		return errors.New("runtime probe omitted original residency/version metadata")
	}
	for index, path := range []string{"/running", "/upstream/" + envelope.Call.Member.Model + "/version", "/running"} {
		item := observation.Metadata[index]
		if item.Path != path || item.Status != http.StatusOK || strings.ToLower(strings.TrimSpace(strings.SplitN(item.ContentType, ";", 2)[0])) != "application/json" || len(item.Body) == 0 || len(item.Body) > defaultWinnerMultipassProviderLimits().MaximumMetadataBytes || !json.Valid(item.Body) {
			return errors.New("runtime probe original metadata path, status, media type or bound changed")
		}
	}
	var version struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(observation.Metadata[1].Body, &version) != nil || version.Version != "0.30.0" || !winnerMultipassRuntimeReady(observation.Metadata[2].Body, envelope.Call.Member) {
		return errors.New("runtime probe did not observe the fixed backend version and sole ready member")
	}
	return nil
}

// Portable validation is original-byte reconstruction, not a trusted JSON flag.
func validateWinnerMultipassRuntimeObservation(envelope winnerMultipassBrokerEnvelope, observation *winnerMultipassRuntimeObservation, request, response []byte) error {
	if envelope.Kind != winnerMultipassRuntimeProbeKind || observation == nil || observation.Schema != 1 || observation.Kind != "winner-review-multipass-runtime-observation/v1" || observation.Member != envelope.Call.Member || observation.CheckpointEvidenceKind != "trusted_deployment_declaration_not_weight_attestation" || observation.DeclaredMaximumContextTokens != envelope.Settings.TokenizationProfiles[envelope.Call.Member.Name].MaximumContextTokens || observation.TokenizerContractSha256 != winnerMultipassTokenizerContractSha256() {
		return errors.New("runtime observation lost its declared synthetic member or overclaims weights")
	}
	if err := validateWinnerMultipassRuntimeMetadata(envelope, observation); err != nil {
		return err
	}
	expected, err := winnerMultipassTokenizerPayload(envelope)
	if err != nil || !bytes.Equal(expected, request) {
		return errors.New("runtime probe tokenizer request changed compiled rendering")
	}
	var decoded struct {
		Count           int      `json:"count"`
		MaximumModelLen int      `json:"max_model_len"`
		Tokens          []int64  `json:"tokens"`
		TokenStrings    []string `json:"token_strs"`
	}
	if decodeStrictJSONBytes(response, &decoded, "original runtime probe tokenizer response") != nil || decoded.Count <= 0 || decoded.Count != len(decoded.Tokens) || decoded.MaximumModelLen != observation.DeclaredMaximumContextTokens || slices.ContainsFunc(decoded.Tokens, func(token int64) bool { return token < 0 }) {
		return errors.New("runtime probe count or context differs from complete original token IDs")
	}
	tokenBytes, _ := json.Marshal(decoded.Tokens)
	if observation.ObservedMaximumContextTokens != decoded.MaximumModelLen || observation.InputTokens != decoded.Count || observation.RenderedPromptSha256 != winnerMultipassSha256(tokenBytes) || observation.TokenizerRequestSha256 != winnerMultipassSha256(request) || observation.TokenizerResponseSha256 != winnerMultipassSha256(response) {
		return errors.New("runtime observation differs from original tokenizer bytes")
	}
	return nil
}
