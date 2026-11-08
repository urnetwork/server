// Offline replay checks the entire planned execution and both attributed finals.
// Missing, ambiguous, late, failed or altered calls cannot authorize any action.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The capture boundary retains the exact request and visible provider terminal.
// Transport hashes bind separately retained protocol evidence; hidden reasoning
// is not requested or stored in this evidence contract.
type winnerMultipassCallEvidence struct {
	CallId                  string                     `json:"call_id"`
	StartedAt               time.Time                  `json:"started_at"`
	CompletedAt             time.Time                  `json:"completed_at"`
	Request                 []byte                     `json:"request"`
	Admission               winnerMultipassAdmission   `json:"admission"`
	VisibleResponse         []byte                     `json:"visible_response"`
	VisibleResponseSha256   string                     `json:"visible_response_sha256"`
	TransportResponseSha256 string                     `json:"transport_response_sha256"`
	TransportReceiptSha256  string                     `json:"transport_receipt_sha256"`
	ProviderOutput          string                     `json:"provider_output"`
	Observation             winnerMultipassObservation `json:"observation"`
	Error                   string                     `json:"error"`
}

// All original calls are retained in exact planned order, including failures.
// There is deliberately no API submission, reward or source authority field.
type winnerMultipassEvidence struct {
	Version      string                        `json:"version"`
	PlanSha256   string                        `json:"plan_sha256"`
	BundleSha256 string                        `json:"bundle_sha256"`
	Calls        []winnerMultipassCallEvidence `json:"calls"`
}

// Parse only a complete visible terminal, requiring exact model identity and
// real usage. A capture implementation must strip no assistant output and must
// not substitute summaries for these original visible bytes.
func winnerMultipassVisibleResponse(evidence winnerMultipassCallEvidence, call winnerMultipassCall, settings winnerMultipassSettings) (*winnerModelResponse, error) {
	if evidence.Error != "" || len(evidence.VisibleResponse) == 0 || len(evidence.VisibleResponse) > settings.MaximumResponseBytes ||
		evidence.VisibleResponseSha256 != winnerMultipassSha256(evidence.VisibleResponse) || !validSha256(evidence.TransportResponseSha256) || !validSha256(evidence.TransportReceiptSha256) {
		return nil, errors.New("multi-pass provider terminal is failed, absent, unbound or oversized")
	}
	var response winnerModelResponse
	if err := decodeStrictJSONBytes(evidence.VisibleResponse, &response, "multi-pass visible terminal"); err != nil {
		return nil, err
	}
	if response.Status != "completed" || response.Model != call.Member.Model || strings.TrimSpace(response.Id) == "" {
		return nil, errors.New("multi-pass terminal model or status is not the planned completed member")
	}
	var usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if json.Unmarshal(response.Usage, &usage) != nil || usage.InputTokens <= 0 || usage.OutputTokens <= 0 || usage.InputTokens > evidence.Admission.InputTokens || usage.OutputTokens > settings.MaximumOutputTokens {
		return nil, errors.New("multi-pass usage is absent or exceeds admitted actual runtime tokens")
	}
	output := ""
	for _, item := range response.Output {
		if item.Type != "message" || item.Role != "assistant" {
			return nil, errors.New("multi-pass visible terminal contains a tool, hidden reasoning or unsupported output")
		}
		for _, content := range item.Content {
			if content.Type != "output_text" || output != "" {
				return nil, errors.New("multi-pass terminal does not contain exactly one complete observation")
			}
			output = content.Text
		}
	}
	if output == "" || output != evidence.ProviderOutput || len(output) > settings.MaximumObservationBytes || rejectWinnerReviewCaseAliases([]byte(output)) != nil {
		return nil, errors.New("multi-pass original visible output is missing, altered or oversized")
	}
	var observation winnerMultipassObservation
	if err := decodeStrictJSONBytes([]byte(output), &observation, "multi-pass original observation"); err != nil {
		return nil, err
	}
	want, _ := json.Marshal(observation)
	actual, _ := json.Marshal(evidence.Observation)
	if !bytes.Equal(want, actual) {
		return nil, errors.New("multi-pass decoded observation differs from original provider output")
	}
	return &response, nil
}

// A prefix can be resumed only from its next unstarted planned call. Durable
// started/ambiguous claim handling belongs to the handoff owner, not this reader.
func validateWinnerMultipassCompletedPrefix(plan *winnerMultipassPlan, bundle winnerMultipassBundle, evidence winnerMultipassEvidence) (map[string]winnerMultipassCallEvidence, error) {
	if err := validateWinnerMultipassPlan(plan, bundle); err != nil {
		return nil, err
	}
	planBytes, _ := json.Marshal(plan)
	if evidence.Version != winnerMultipassVersion || evidence.PlanSha256 != winnerMultipassSha256(planBytes) || evidence.BundleSha256 != plan.BundleSha256 ||
		evidence.Calls == nil || len(evidence.Calls) > len(plan.Calls) {
		return nil, errors.New("multi-pass evidence does not bind its exact immutable plan")
	}
	completed := map[string]winnerMultipassCallEvidence{}
	responseIds := map[string]bool{}
	previousCompletion := plan.Owner.StartedAt
	for index, observation := range evidence.Calls {
		call := plan.Calls[index]
		if observation.CallId != call.Id || observation.StartedAt.Before(previousCompletion) || observation.CompletedAt.Before(observation.StartedAt) || !observation.CompletedAt.Before(plan.Owner.Deadline) {
			return nil, fmt.Errorf("multi-pass call %d is reordered, concurrent, late or outside its original owner clock", index+1)
		}
		payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, completed)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(payload, observation.Request) {
			return nil, errors.New("multi-pass call prompt differs from its exact planned source and predecessors")
		}
		if err := validateWinnerMultipassAdmission(plan, call, payload, observation.Admission); err != nil {
			return nil, err
		}
		response, err := winnerMultipassVisibleResponse(observation, call, plan.Settings)
		if err != nil {
			return nil, err
		}
		if responseIds[response.Id] {
			return nil, errors.New("multi-pass provider response identity was reused")
		}
		responseIds[response.Id] = true
		issues, err := winnerMultipassInheritedIssues(plan, call, completed)
		if err != nil {
			return nil, err
		}
		if err := validateWinnerMultipassObservation(plan, bundle, call, observation.Observation, issues); err != nil {
			return nil, err
		}
		completed[call.Id] = observation
		previousCompletion = observation.CompletedAt
	}
	return completed, nil
}

// This validation returns evidence-derived judgment only. Even unanimous support
// does not post approval, release holds, promote source or distribute rewards.
func validateWinnerMultipassEvidence(plan *winnerMultipassPlan, bundle winnerMultipassBundle, evidence winnerMultipassEvidence) (winnerModelJudgment, error) {
	unresolved := winnerModelJudgment{Decision: "uncertain", ReasonCodes: []string{"SOURCE_NOT_READY"}, PublicExplanation: "Complete multi-pass review evidence is unresolved; no approval is authorized."}
	completed, err := validateWinnerMultipassCompletedPrefix(plan, bundle, evidence)
	if err != nil {
		return unresolved, err
	}
	if len(evidence.Calls) != len(plan.Calls) {
		return unresolved, errors.New("multi-pass evidence does not cover all planned calls for both members")
	}
	members := []winnerSnowMemberEvidence{}
	for _, member := range plan.Provider.Members {
		callId := member.Name + "-final"
		item, found := completed[callId]
		if !found || item.Observation.FinalJudgment == nil {
			return unresolved, errors.New("multi-pass member final is missing")
		}
		var response winnerModelResponse
		if json.Unmarshal(item.VisibleResponse, &response) != nil {
			return unresolved, errors.New("multi-pass final terminal cannot be decoded")
		}
		var original map[string]json.RawMessage
		if json.Unmarshal([]byte(item.ProviderOutput), &original) != nil {
			return unresolved, errors.New("multi-pass final original cannot be decoded")
		}
		members = append(members, winnerSnowMemberEvidence{Name: member.Name, RequestedModel: member.Model, Model: response.Model, Revision: member.Revision, Reasoning: member.Reasoning,
			ProviderResponseId: response.Id, RequestSha256: winnerMultipassSha256(item.Request), ResponseSha256: item.TransportResponseSha256, Usage: response.Usage,
			Judgment: *item.Observation.FinalJudgment, ProviderOutput: string(original["final_judgment"])})
	}
	combined := aggregateWinnerSnow(members, plan.Provider, bundle.Request)
	if len(combined.Findings) != 12 {
		return unresolved, errors.New("multi-pass finals do not preserve twelve original attributed findings")
	}
	return combined, nil
}
