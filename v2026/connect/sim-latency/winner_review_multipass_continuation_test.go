// Synthetic interruption tests use the actual complete-prefix replay validator;
// no provider is contacted and no production identity is embedded.
package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"
)

// A complete call's custody is represented synthetically; integration must
// independently retrieve and authenticate protected originals before resuming.
func winnerMultipassContinuationFixture(t *testing.T) (winnerMultipassBundle, *winnerMultipassPlan, fstest.MapFS) {
	t.Helper()
	bundle, plan, judgment := winnerMultipassFixture(t)
	evidence := winnerMultipassEvidenceFixture(t, bundle, plan, judgment)
	planBytes, _ := json.Marshal(plan)
	itemBytes, _ := json.Marshal(evidence.Calls[0])
	ownerBytes, _ := json.Marshal(map[string]any{"owner": plan.Owner})
	claimBytes, _ := json.Marshal(map[string]any{"owner": plan.Owner, "call_id": plan.Calls[0].Id, "request_sha256": winnerMultipassSha256(evidence.Calls[0].Request), "token_admission": evidence.Calls[0].Admission})
	callId := plan.Calls[0].Id
	return bundle, plan, fstest.MapFS{
		"owner.json":                      &fstest.MapFile{Data: ownerBytes},
		"plan.json":                       &fstest.MapFile{Data: planBytes},
		callId + "-evidence.json":         &fstest.MapFile{Data: itemBytes},
		callId + "-generation-claim.json": &fstest.MapFile{Data: claimBytes},
		callId + "-tokenize":              &fstest.MapFile{Mode: fs.ModeDir},
		callId + "-generate":              &fstest.MapFile{Mode: fs.ModeDir},
	}
}

// Interruption exactly between calls retains the first result and selects only
// the next unstarted call, without modifying the original owner or deadline.
func TestWinnerMultipassContinuationPreservesCompletedPrefix(t *testing.T) {
	bundle, plan, files := winnerMultipassContinuationFixture(t)
	originalOwner := plan.Owner
	verified := 0
	evidence, err := readWinnerMultipassContinuation(plan, bundle, files, plan.Owner.StartedAt.Add(time.Minute), func(item winnerMultipassCallEvidence) error {
		verified++
		if item.CallId != plan.Calls[0].Id {
			t.Fatal("wrong authenticated call")
		}
		return nil
	})
	if err != nil || len(evidence.Calls) != 1 || verified != 1 || plan.Owner != originalOwner {
		t.Fatal("completed prefix was lost or its clock changed", err)
	}
}

// Any started but incomplete call must stop recovery, even if only tokenization
// is visible: absent generation evidence cannot prove that a draw did not occur.
func TestWinnerMultipassContinuationRefusesAmbiguousNextCall(t *testing.T) {
	for _, suffix := range []string{"-generation-claim.json", "-tokenize", "-generate"} {
		bundle, plan, files := winnerMultipassContinuationFixture(t)
		mode := fs.FileMode(0)
		if suffix != "-generation-claim.json" {
			mode = fs.ModeDir
		}
		files[plan.Calls[1].Id+suffix] = &fstest.MapFile{Mode: mode}
		if _, err := readWinnerMultipassContinuation(plan, bundle, files, plan.Owner.StartedAt.Add(time.Minute), func(winnerMultipassCallEvidence) error { return nil }); err == nil {
			t.Fatalf("ambiguous next call accepted: %s", suffix)
		}
	}
}

// Neither a changed clock observation nor failed independent storage readback
// may turn a retained local record into permission to send another request.
func TestWinnerMultipassContinuationRefusesExpiredOrUnauthenticatedEvidence(t *testing.T) {
	bundle, plan, files := winnerMultipassContinuationFixture(t)
	for _, now := range []time.Time{plan.Owner.StartedAt.Add(-time.Second), plan.Owner.Deadline, plan.Owner.Deadline.Add(time.Second)} {
		if _, err := readWinnerMultipassContinuation(plan, bundle, files, now, func(winnerMultipassCallEvidence) error { t.Fatal("authentication after expired owner"); return nil }); err == nil {
			t.Fatal("owner clock renewed")
		}
	}
	if _, err := readWinnerMultipassContinuation(plan, bundle, files, plan.Owner.StartedAt.Add(time.Minute), func(winnerMultipassCallEvidence) error { return errors.New("synthetic readback mismatch") }); err == nil {
		t.Fatal("unverified originals accepted")
	}
	if _, err := readWinnerMultipassContinuation(plan, bundle, files, plan.Owner.StartedAt.Add(time.Minute), nil); err == nil {
		t.Fatal("missing verifier accepted")
	}
}

// Terminal markers, linked records, missing custody and changed plans are
// rejected deterministically, without relying on subprocess timing.
func TestWinnerMultipassContinuationRefusesAlteredOrTerminalInventory(t *testing.T) {
	for _, mutation := range []string{"terminal", "link", "plan", "owner", "claim", "custody", "gap"} {
		bundle, plan, files := winnerMultipassContinuationFixture(t)
		switch mutation {
		case "terminal":
			files["failure.json"] = &fstest.MapFile{Data: []byte("synthetic failure")}
		case "link":
			files["plan.json"].Mode = fs.ModeSymlink
		case "plan":
			files["plan.json"].Data = []byte("changed")
		case "owner":
			files["owner.json"].Data = []byte(`{"owner":null}`)
		case "claim":
			files[plan.Calls[0].Id+"-generation-claim.json"].Data = []byte(`{}`)
		case "custody":
			delete(files, plan.Calls[0].Id+"-generation-claim.json")
		case "gap":
			files[plan.Calls[1].Id+"-evidence.json"] = files[plan.Calls[0].Id+"-evidence.json"]
			delete(files, plan.Calls[0].Id+"-evidence.json")
		}
		if _, err := readWinnerMultipassContinuation(plan, bundle, files, plan.Owner.StartedAt.Add(time.Minute), func(winnerMultipassCallEvidence) error { return nil }); err == nil {
			t.Fatalf("accepted %s", mutation)
		}
	}
}
