// Non-inference preparation batches a fixed member across the complete closed
// corpus. It does not prequalify a release or authorize a later provider draw.
package main

import (
	"context"
	"errors"
	"fmt"
)

// The exact current case plan supplies the owner/settings/plan hash needed by
// the contained tokenizer envelope. The native adapter treats it as read-only
// and retains every original wrapper/renderer response separately from counts.
type winnerMultipassQualificationTokenizer func(context.Context, int, *winnerMultipassPlan, winnerMultipassCall, []byte) (winnerMultipassAdmission, error)

type winnerMultipassQualificationProbe struct {
	CasePosition int                      `json:"case_position"`
	Admission    winnerMultipassAdmission `json:"admission"`
}

// Partial results preserve every attempted probe, including the terminal error.
// No approved/qualified bit is provided; only the genuine executor can qualify.
type winnerMultipassQualificationPacking struct {
	Plan            *winnerMultipassQualificationPlan   `json:"plan"`
	Probes          []winnerMultipassQualificationProbe `json:"probes"`
	FinalAdmissions []winnerMultipassQualificationProbe `json:"final_admissions"`
}

// Initial complete one-leaf layouts are deliberately UNADMITTED. They allow
// prospective packing even if one-atom layouts exceed the group call ceiling,
// without rendering or dispatching those oversized one-leaf payloads. Every
// actual request is subsequently grown from bounded atoms using real counts.
func initialWinnerMultipassQualificationPacking(requests []winnerReviewBrokerRequest, owner winnerMultipassOwner, settings winnerMultipassSettings, release winnerMultipassQualificationRelease) (*winnerMultipassQualificationPlan, error) {
	if len(requests) != 19 {
		return nil, errors.New("prospective group packing requires all nineteen original controls")
	}
	groups := [][]winnerMultipassScanGroup{}
	for index, request := range requests {
		request.Operation = "review_elected_winner"
		bundle := winnerMultipassBundle{Request: request, SupplementalFiles: []winnerMultipassSourceFile{}, RequiredSupplementalPaths: []string{}, UnresolvedContext: []string{}}
		caseOwner := winnerMultipassOwner{Id: winnerMultipassSha256([]byte(fmt.Sprintf("%s/control-%02d", owner.Id, index+1))), StartedAt: owner.StartedAt, Deadline: owner.Deadline}
		plan, err := prepareWinnerMultipass(bundle, caseOwner, settings, defaultWinnerSnowProviderSettings())
		if err != nil {
			return nil, err
		}
		if index == 0 {
			groups = append(groups, plan.ScanGroups)
			continue
		}
		ids := []string{}
		for _, unit := range plan.Units {
			ids = append(ids, unit.Id)
		}
		groups = append(groups, []winnerMultipassScanGroup{{Member: plan.Provider.Members[0].Name, UnitIds: append([]string{}, ids...)}, {Member: plan.Provider.Members[1].Name, UnitIds: append([]string{}, ids...)}})
	}
	return prepareWinnerMultipassQualification(requests, owner, settings, release, groups)
}

// One Qwen batch then one Gemma batch covers every generated control. After all
// partitions are fixed, a second member-batched pass counts exact finalized
// payloads. The later native frozen-owner preflight remains mandatory: changing
// an owner/clock/plan hash cannot borrow a placeholder prompt's admission.
func packWinnerMultipassQualification(ctx context.Context, requests []winnerReviewBrokerRequest, owner winnerMultipassOwner, settings winnerMultipassSettings, release winnerMultipassQualificationRelease, tokenizer winnerMultipassQualificationTokenizer) (*winnerMultipassQualificationPacking, error) {
	if tokenizer == nil {
		return nil, errors.New("prospective group packing requires a qualified contained tokenizer")
	}
	initial, err := initialWinnerMultipassQualificationPacking(requests, owner, settings, release)
	if err != nil {
		return nil, err
	}
	result := &winnerMultipassQualificationPacking{Plan: initial, Probes: []winnerMultipassQualificationProbe{}, FinalAdmissions: []winnerMultipassQualificationProbe{}}
	groups := make([][]winnerMultipassScanGroup, len(initial.Cases))
	groups[0] = initial.Cases[0].Plan.ScanGroups
	for _, member := range defaultWinnerSnowProviderSettings().Members {
		for _, item := range initial.Cases[1:] {
			memberGroups, probes, err := packWinnerMultipassMember(ctx, item.Plan, item.Bundle, member, func(ctx context.Context, call winnerMultipassCall, payload []byte) (winnerMultipassAdmission, error) {
				return tokenizer(ctx, item.Position, item.Plan, call, payload)
			})
			for _, admission := range probes {
				result.Probes = append(result.Probes, winnerMultipassQualificationProbe{CasePosition: item.Position, Admission: admission})
			}
			if err != nil {
				return result, err
			}
			groups[item.Position-1] = append(groups[item.Position-1], memberGroups...)
		}
	}
	final, err := prepareWinnerMultipassQualification(requests, owner, settings, release, groups)
	if err != nil {
		return result, err
	}
	result.Plan = final
	for _, member := range defaultWinnerSnowProviderSettings().Members {
		for _, item := range final.Cases[1:] {
			for _, call := range item.Plan.Calls {
				if call.Member != member || call.Stage != "scan" {
					continue
				}
				if err := ctx.Err(); err != nil {
					return result, err
				}
				payload, err := buildWinnerMultipassCall(item.Plan, item.Bundle, call.Id, map[string]winnerMultipassCallEvidence{})
				if err != nil {
					return result, err
				}
				admission, err := tokenizer(ctx, item.Position, item.Plan, call, payload)
				result.FinalAdmissions = append(result.FinalAdmissions, winnerMultipassQualificationProbe{CasePosition: item.Position, Admission: admission})
				if err != nil {
					return result, err
				}
				if err := validateWinnerMultipassAdmission(item.Plan, call, payload, admission); err != nil {
					return result, err
				}
			}
		}
	}
	return result, validateWinnerMultipassQualificationPacking(result)
}

// Exact complete final-leaf admission is distinct from the earlier grow probes.
// An omitted, reordered, duplicate or cross-case count never admits a plan.
func validateWinnerMultipassQualificationPacking(packing *winnerMultipassQualificationPacking) error {
	if packing == nil || packing.Plan == nil || packing.Probes == nil || packing.FinalAdmissions == nil {
		return errors.New("prospective group packing is incomplete")
	}
	if err := validateWinnerMultipassQualificationPlan(packing.Plan); err != nil {
		return err
	}
	index := 0
	for _, member := range defaultWinnerSnowProviderSettings().Members {
		for _, item := range packing.Plan.Cases[1:] {
			for _, call := range item.Plan.Calls {
				if call.Member != member || call.Stage != "scan" {
					continue
				}
				if index >= len(packing.FinalAdmissions) || packing.FinalAdmissions[index].CasePosition != item.Position {
					return errors.New("prospective group final-leaf census/order changed")
				}
				payload, err := buildWinnerMultipassCall(item.Plan, item.Bundle, call.Id, map[string]winnerMultipassCallEvidence{})
				if err != nil {
					return err
				}
				if err := validateWinnerMultipassAdmission(item.Plan, call, payload, packing.FinalAdmissions[index].Admission); err != nil {
					return err
				}
				index++
			}
		}
	}
	if index != len(packing.FinalAdmissions) {
		return errors.New("prospective group final-leaf census has extra admissions")
	}
	return nil
}
