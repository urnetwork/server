// A read-only continuation preflight identifies the next genuinely unstarted
// call. It never sends requests, renews an owner, or approves a review.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// The caller supplies an authenticated immutable plan and an independent
// original-evidence verifier. A successful read is not execution authority.
func readWinnerMultipassContinuation(plan *winnerMultipassPlan, bundle winnerMultipassBundle, files fs.FS, now time.Time, authenticate func(winnerMultipassCallEvidence) error) (winnerMultipassEvidence, error) {
	planBytes, err := json.Marshal(plan)
	evidence := winnerMultipassEvidence{Version: winnerMultipassVersion, PlanSha256: winnerMultipassSha256(planBytes), Calls: []winnerMultipassCallEvidence{}}
	if err != nil || plan == nil || files == nil || authenticate == nil {
		return evidence, errors.New("continuation requires a plan and independent original authentication")
	}
	evidence.BundleSha256 = plan.BundleSha256
	if now.Before(plan.Owner.StartedAt) || !now.Before(plan.Owner.Deadline) {
		return evidence, errors.New("continuation is outside the original owner clock")
	}
	if err := validateWinnerMultipassPlan(plan, bundle); err != nil {
		return evidence, err
	}
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return evidence, err
	}
	allowed := map[string]bool{"owner.json": true, "plan.json": true}
	for _, call := range plan.Calls {
		for _, suffix := range []string{"-evidence.json", "-generation-claim.json", "-tokenize", "-generate"} {
			allowed[call.Id+suffix] = true
		}
	}
	present := map[string]bool{}
	for _, entry := range entries {
		if !allowed[entry.Name()] || entry.Type()&fs.ModeSymlink != 0 {
			return evidence, fmt.Errorf("continuation refuses terminal, unknown or linked artifact %q", entry.Name())
		}
		wantDirectory := strings.HasSuffix(entry.Name(), "-tokenize") || strings.HasSuffix(entry.Name(), "-generate")
		if wantDirectory != entry.IsDir() {
			return evidence, fmt.Errorf("continuation artifact type differs: %s", entry.Name())
		}
		present[entry.Name()] = true
	}
	storedPlan, err := fs.ReadFile(files, "plan.json")
	if err != nil || winnerMultipassSha256(storedPlan) != evidence.PlanSha256 {
		return evidence, errors.New("continuation plan differs from the authenticated plan")
	}
	ownerBytes, err := fs.ReadFile(files, "owner.json")
	var ownerFields map[string]json.RawMessage
	var storedOwner winnerMultipassOwner
	if err != nil || decodeStrictJSONBytes(ownerBytes, &ownerFields, "continuation owner") != nil ||
		decodeStrictJSONBytes(ownerFields["owner"], &storedOwner, "continuation owner clock") != nil || storedOwner != plan.Owner {
		return evidence, errors.New("continuation owner differs from the original clock")
	}
	gap := false
	for _, call := range plan.Calls {
		if !present[call.Id+"-evidence.json"] {
			gap = true
			for _, suffix := range []string{"-generation-claim.json", "-tokenize", "-generate"} {
				if present[call.Id+suffix] {
					return evidence, fmt.Errorf("continuation refuses incomplete or ambiguous call %s", call.Id)
				}
			}
			continue
		}
		if gap {
			return evidence, errors.New("continuation evidence is not a contiguous prefix")
		}
		for _, suffix := range []string{"-generation-claim.json", "-tokenize", "-generate"} {
			if !present[call.Id+suffix] {
				return evidence, fmt.Errorf("continuation lacks original custody for %s", call.Id)
			}
		}
		content, err := fs.ReadFile(files, call.Id+"-evidence.json")
		if err != nil {
			return evidence, err
		}
		var item winnerMultipassCallEvidence
		if err := decodeStrictJSONBytes(content, &item, "continuation original call evidence"); err != nil {
			return evidence, err
		}
		if item.CallId != call.Id || item.CompletedAt.After(now) {
			return evidence, errors.New("continuation call identity or observation time differs")
		}
		claimBytes, err := fs.ReadFile(files, call.Id+"-generation-claim.json")
		var claim struct {
			Owner         winnerMultipassOwner     `json:"owner"`
			CallId        string                   `json:"call_id"`
			RequestSha256 string                   `json:"request_sha256"`
			Admission     winnerMultipassAdmission `json:"token_admission"`
		}
		if err != nil || decodeStrictJSONBytes(claimBytes, &claim, "continuation original claim") != nil ||
			claim.Owner != plan.Owner || claim.CallId != call.Id || claim.RequestSha256 != winnerMultipassSha256(item.Request) || claim.Admission != item.Admission {
			return evidence, errors.New("continuation original claim differs from its completed call")
		}
		if err := authenticate(item); err != nil {
			return evidence, err
		}
		evidence.Calls = append(evidence.Calls, item)
	}
	if _, err := validateWinnerMultipassCompletedPrefix(plan, bundle, evidence); err != nil {
		return evidence, err
	}
	return evidence, nil
}
