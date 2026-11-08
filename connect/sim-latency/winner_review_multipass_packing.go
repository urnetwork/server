// Prospective token packing is separate from execution. It may query only an
// approved non-inference tokenizer and cannot consume or reopen a review attempt.
package main

import (
	"context"
	"errors"
	"fmt"
)

// The qualified adapter retains exact rendering/request/response proofs for all
// probes, including over-limit counts. An error is terminal, never retried here.
type winnerMultipassTokenizer func(context.Context, winnerMultipassCall, []byte) (winnerMultipassAdmission, error)

// Packing records every prospective count and the exact final leaf admissions.
// Intermediate merge/final inputs are admitted later, from their fixed children.
type winnerMultipassPacking struct {
	Plan            *winnerMultipassPlan       `json:"plan"`
	Probes          []winnerMultipassAdmission `json:"probes"`
	FinalAdmissions []winnerMultipassAdmission `json:"final_admissions"`
}

// Greedily pack contiguous complete units separately per member using actual
// full-prompt counts. A final re-render/recount removes the plan-digest cycle.
// Callers must use this only before durable owner/call freezing; it performs no
// review inference and has no resume/replan path for a started review.
func packWinnerMultipass(ctx context.Context, initial *winnerMultipassPlan, bundle winnerMultipassBundle, tokenizer winnerMultipassTokenizer) (*winnerMultipassPacking, error) {
	if tokenizer == nil {
		return nil, errors.New("prospective packing requires a qualified non-inference tokenizer")
	}
	if err := validateWinnerMultipassPlan(initial, bundle); err != nil {
		return nil, err
	}
	plan := *initial
	result := &winnerMultipassPacking{Plan: &plan, Probes: []winnerMultipassAdmission{}, FinalAdmissions: []winnerMultipassAdmission{}}
	groups := []winnerMultipassScanGroup{}
	for _, member := range plan.Provider.Members {
		index := 0
		groupNumber := 0
		for index < len(plan.Units) {
			groupNumber++
			accepted := []string{}
			for end := index; end < len(plan.Units); end++ {
				if err := ctx.Err(); err != nil {
					return result, err
				}
				candidate := append(append([]string{}, accepted...), plan.Units[end].Id)
				call := winnerMultipassCall{Id: fmt.Sprintf("%s-scan-%04d", member.Name, groupNumber), Member: member, Stage: "scan", UnitIds: candidate, Parents: []string{}}
				payload, err := winnerMultipassCallPayload(&plan, bundle, call, map[string]winnerMultipassCallEvidence{})
				if err != nil {
					return result, err
				}
				admission, err := tokenizer(ctx, call, payload)
				result.Probes = append(result.Probes, admission)
				if err != nil {
					return result, err
				}
				if err := validateWinnerMultipassTokenization(&plan, call, payload, admission); err != nil {
					return result, err
				}
				if admission.InputTokens > admission.MaximumContextTokens-admission.OutputTokens {
					break
				}
				accepted = candidate
			}
			if len(accepted) == 0 {
				return result, errors.New("one complete coverage atom cannot fit; prospective source segmentation must be revised before any inference")
			}
			groups = append(groups, winnerMultipassScanGroup{Member: member.Name, UnitIds: accepted})
			index += len(accepted)
		}
	}
	if err := setWinnerMultipassScanGroups(&plan, groups); err != nil {
		return result, err
	}
	if err := validateWinnerMultipassPlan(&plan, bundle); err != nil {
		return result, err
	}
	for _, call := range plan.Calls {
		if call.Stage != "scan" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		payload, err := buildWinnerMultipassCall(&plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
		if err != nil {
			return result, err
		}
		admission, err := tokenizer(ctx, call, payload)
		result.FinalAdmissions = append(result.FinalAdmissions, admission)
		if err != nil {
			return result, err
		}
		if err := validateWinnerMultipassAdmission(&plan, call, payload, admission); err != nil {
			return result, err
		}
	}
	return result, nil
}

// An owner may freeze only after all exact final scan payloads have independent
// current-runtime admission. No byte heuristic substitutes for a missing count.
func validateWinnerMultipassLeafAdmissions(plan *winnerMultipassPlan, bundle winnerMultipassBundle, admissions []winnerMultipassAdmission) error {
	if err := validateWinnerMultipassPlan(plan, bundle); err != nil {
		return err
	}
	index := 0
	for _, call := range plan.Calls {
		if call.Stage != "scan" {
			continue
		}
		if index >= len(admissions) {
			return errors.New("multi-pass leaf token preflight is incomplete")
		}
		payload, err := buildWinnerMultipassCall(plan, bundle, call.Id, map[string]winnerMultipassCallEvidence{})
		if err != nil {
			return err
		}
		if err := validateWinnerMultipassAdmission(plan, call, payload, admissions[index]); err != nil {
			return err
		}
		index++
	}
	if index != len(admissions) {
		return errors.New("multi-pass leaf token preflight contains extra or duplicate admissions")
	}
	return nil
}
