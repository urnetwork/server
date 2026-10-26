// Image preparation must preserve explicit candidate semantics across winner
// promotion, never silently reverting a broad source image to server-only.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// New image locks opt in explicitly; legacy requests omit the new format field.
func TestSourceBuildImagePreservesExplicitCandidatePathBase(t *testing.T) {
	request := sourceBuildContextFixture(t)
	request.Proposal.CandidatePathBase = "mono_root_v1"
	plan, err := sourceBuildImagePlanForDigest(request, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	var lock evaluatorSourceLock
	if err := json.Unmarshal(plan.SourceLock, &lock); err != nil || lock.CandidatePathBase != "mono_root_v1" {
		t.Fatalf("broad source capability lost in image: %v", err)
	}
	request.Proposal.CandidatePathBase = ""
	legacy, err := sourceBuildImagePlanForDigest(request, strings.Repeat("a", 64))
	if err != nil || bytes.Contains(legacy.SourceLock, []byte("candidate_path_base")) {
		t.Fatalf("legacy source image was relabeled: %v", err)
	}
}

// An unknown semantic flag must fail before cloning or building anything.
func TestSourceBuildRejectsUnknownCandidatePathBase(t *testing.T) {
	request := sourceBuildContextFixture(t)
	request.Proposal.CandidatePathBase = "unsupported_mode"
	if err := validateSourceBuildRequest(request); err == nil || !strings.Contains(err.Error(), "candidate path base") {
		t.Fatalf("unknown candidate semantics accepted: %v", err)
	}
}
