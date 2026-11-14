// Response failures retain only bounded protocol metadata. Provider output,
// hidden reasoning, raw errors and response identifiers never enter diagnostics.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Machine-readable rejection details distinguish truncation, model mismatch
// and missing framing without granting partial responses judgment authority.
type winnerModelResponseDiagnostic struct {
	Cause                  string `json:"cause"`
	TerminalEvent          string `json:"terminal_event"`
	Status                 string `json:"status"`
	IncompleteReason       string `json:"incomplete_reason"`
	RequestedModel         string `json:"requested_model"`
	ReturnedModel          string `json:"returned_model"`
	IdPresent              bool   `json:"id_present"`
	ResponseEnvelopeSha256 string `json:"response_envelope_sha256"`
}

// Only fixed field names and bounded protocol atoms are serialized; no raw
// provider error string or output text is included in this retained message.
func (self *winnerModelResponseDiagnostic) Error() string {
	content, _ := json.Marshal(self)
	return "review provider response rejected: " + string(content)
}

// Metadata decoding deliberately ignores output/reasoning/error payloads.
type winnerModelResponseIdentity struct {
	Status            string `json:"status"`
	Model             string `json:"model"`
	Id                string `json:"id"`
	IncompleteDetails struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

// Reject invalid, missing or oversized atoms as a whole rather than leaking an
// arbitrary provider-controlled prefix. Identifier content is never retained.
func winnerModelResponseFailure(cause, event string, content json.RawMessage, requestedModel string, settings winnerModelSettings) error {
	var identity winnerModelResponseIdentity
	_ = json.Unmarshal(content, &identity)
	atom := func(value string) string {
		if len(value) > settings.MaximumDiagnosticFieldBytes || settings.MaximumDiagnosticFieldBytes <= 0 {
			return "[oversized]"
		}
		for _, c := range value {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.' || c == '/' || c == ':') {
				return "[invalid]"
			}
		}
		return value
	}
	digest := ""
	if len(content) > 0 {
		value := sha256.Sum256(content)
		digest = hex.EncodeToString(value[:])
	}
	return &winnerModelResponseDiagnostic{Cause: cause, TerminalEvent: event, Status: atom(identity.Status), IncompleteReason: atom(identity.IncompleteDetails.Reason),
		RequestedModel: atom(requestedModel), ReturnedModel: atom(identity.Model), IdPresent: identity.Id != "", ResponseEnvelopeSha256: digest}
}

// Both JSON responses and SSE terminal envelopes must name the exact model,
// completed status and a response id. An incomplete terminal is never accepted.
func validateWinnerModelResponseIdentity(content json.RawMessage, requestedModel, terminalEvent string, settings winnerModelSettings) error {
	var identity winnerModelResponseIdentity
	if json.Unmarshal(content, &identity) != nil || bytes.Equal(bytes.TrimSpace(content), []byte("null")) {
		return winnerModelResponseFailure("malformed_terminal_response", terminalEvent, content, requestedModel, settings)
	}
	if identity.Status != "completed" {
		return winnerModelResponseFailure("status_not_completed", terminalEvent, content, requestedModel, settings)
	}
	if identity.Model != requestedModel {
		return winnerModelResponseFailure("model_mismatch", terminalEvent, content, requestedModel, settings)
	}
	if identity.Id == "" {
		return winnerModelResponseFailure("missing_response_id", terminalEvent, content, requestedModel, settings)
	}
	return nil
}
