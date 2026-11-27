package controller

// The compute staff kinds (CARCH-M12-01): compute_suspend and compute_resume read their bodies strictly, refuse what
// they cannot take before any act, run for a reviewer alone, carry the reviewer's id, and refuse in a binary without
// MG-C1. The acts themselves run on the private stores in computet3's compute_abuse_hold_t3_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	sidecarmodel "github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// The acts a test sees, and what they answer.
type testAbuseComputeHoldActs struct {
	requests []*sidecarcontroller.ComputeAbuseHoldRequest
	kinds    []string
	answer   *sidecarcontroller.ComputeAbuseHoldAnswer
	err      error
}

// Replaces the kinds' deps for the test, linked, with acts that record and answer as acts says.
func installTestAbuseComputeHoldActs(t *testing.T, linked bool) *testAbuseComputeHoldActs {
	saved := abuseComputeHoldDeps
	t.Cleanup(func() { abuseComputeHoldDeps = saved })
	acts := &testAbuseComputeHoldActs{}
	record := func(kind string) func(env operator.Env, ctx context.Context, settings *sidecarmodel.ComputeAbuseHoldSettings, request *sidecarcontroller.ComputeAbuseHoldRequest) (*sidecarcontroller.ComputeAbuseHoldAnswer, error) {
		return func(env operator.Env, ctx context.Context, settings *sidecarmodel.ComputeAbuseHoldSettings, request *sidecarcontroller.ComputeAbuseHoldRequest) (*sidecarcontroller.ComputeAbuseHoldAnswer, error) {
			if settings == nil || settings.ProjectionMaxAge <= 0 {
				t.Errorf("%s ran with settings %+v", kind, settings)
			}
			copied := *request
			acts.requests = append(acts.requests, &copied)
			acts.kinds = append(acts.kinds, kind)
			return acts.answer, acts.err
		}
	}
	abuseComputeHoldDeps.linked = func() bool { return linked }
	abuseComputeHoldDeps.env = func() operator.Env { return nil }
	abuseComputeHoldDeps.suspend = record("suspend")
	abuseComputeHoldDeps.resume = record("resume")
	return acts
}

// compute_suspend holds the network named for the reason named, as the reviewer; compute_resume ends it; each answers
// the hold's view, with no field the network's own.
func TestAbuseComputeKindsRunTheirActs(t *testing.T) {
	ctx := context.Background()
	acts := installTestAbuseComputeHoldActs(t, true)
	reviewer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
	networkId := server.NewId()
	holdTime := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	acts.answer = &sidecarcontroller.ComputeAbuseHoldAnswer{
		Hold: &sidecarmodel.ComputeAbuseHold{
			NetworkId: operator.Id(networkId), Reason: sidecarmodel.ComputeAbuseHoldReasonMining,
			HoldStaffId: operator.Id(reviewer.ReviewerId), HoldTime: holdTime, Version: 1,
		},
		Changed: true, Projected: true, Enclaves: sidecarcontroller.ComputeAbuseHoldEnclavesNone,
	}
	result, err := AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"compute_suspend","network_id":"`+networkId.String()+`","reason":"mining"}`))
	if err != nil {
		t.Fatal(err)
	}
	view, ok := result.Result.(*AbuseStaffComputeHoldView)
	if result.Kind != AbuseStaffKindComputeSuspend || !ok || view.NetworkId != networkId || !view.Held || view.Reason != "mining" ||
		view.Version != 1 || view.HoldTimeMs != holdTime.UnixMilli() || view.ResumeTimeMs != 0 || !view.Changed || !view.Projected || view.Enclaves != "none" {
		t.Fatalf("the suspension answered %+v %+v", result, view)
	}
	request := acts.requests[0]
	if acts.kinds[0] != "suspend" || request.NetworkId != operator.Id(networkId) || request.Reason != sidecarmodel.ComputeAbuseHoldReasonMining ||
		request.StaffId != operator.Id(reviewer.ReviewerId) {
		t.Fatalf("the act was asked %+v", request)
	}

	resumeTime := holdTime.Add(time.Hour)
	resumeStaffId := operator.Id(reviewer.ReviewerId)
	acts.answer.Hold.ResumeTime, acts.answer.Hold.ResumeStaffId, acts.answer.Hold.Version = &resumeTime, &resumeStaffId, 2
	acts.answer.Enclaves = sidecarcontroller.ComputeAbuseHoldEnclavesUntouched
	result, err = AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"compute_resume","network_id":"`+networkId.String()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	view = result.Result.(*AbuseStaffComputeHoldView)
	if view.Held || view.Version != 2 || view.ResumeTimeMs != resumeTime.UnixMilli() || view.Enclaves != "untouched" || acts.kinds[1] != "resume" ||
		acts.requests[1].StaffId != operator.Id(reviewer.ReviewerId) {
		t.Fatalf("the resume answered %+v, asked %+v", view, acts.requests[1])
	}

	// a resume of a network never held answers no hold
	acts.answer = &sidecarcontroller.ComputeAbuseHoldAnswer{Projected: true, Enclaves: sidecarcontroller.ComputeAbuseHoldEnclavesUntouched}
	result, err = AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"compute_resume","network_id":"`+server.NewId().String()+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if view := result.Result.(*AbuseStaffComputeHoldView); view.Held || view.Reason != "" || view.Version != 0 || view.Changed {
		t.Fatalf("a network never held answered %+v", view)
	}
}

// What the kinds cannot take is refused invalid before any act; a role other than reviewer is refused; a binary
// without MG-C1 refuses, as does an act's own refusal of its arguments.
func TestAbuseComputeKindsRefuseBeforeActing(t *testing.T) {
	ctx := context.Background()
	acts := installTestAbuseComputeHoldActs(t, true)
	reviewer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
	networkId := server.NewId().String()
	for _, body := range []string{
		`{"kind":"compute_suspend","network_id":"` + networkId + `"}`,
		`{"kind":"compute_suspend","network_id":"` + networkId + `","reason":"none"}`,
		`{"kind":"compute_suspend","network_id":"` + networkId + `","reason":"Mining"}`,
		`{"kind":"compute_suspend","network_id":"not an id","reason":"mining"}`,
		`{"kind":"compute_suspend","network_id":"00000000-0000-0000-0000-000000000000","reason":"mining"}`,
		`{"kind":"compute_suspend","reason":"mining"}`,
		`{"kind":"compute_suspend","network_id":"` + networkId + `","reason":"mining","enclaves":"keep"}`,
		`{"kind":"compute_resume"}`,
		`{"kind":"compute_resume","network_id":"` + networkId + `","reason":"mining"}`,
	} {
		if _, err := AbuseStaffAction(ctx, reviewer, []byte(body)); !errors.Is(err, ErrAbuseStaffRequestInvalid) {
			t.Errorf("%s: %v", body, err)
		}
	}
	legal := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleLegal, AbuseStaffRoleEngineering}}
	for _, body := range []string{
		`{"kind":"compute_suspend","network_id":"` + networkId + `","reason":"takedown"}`,
		`{"kind":"compute_resume","network_id":"` + networkId + `"}`,
	} {
		if _, err := AbuseStaffAction(ctx, legal, []byte(body)); !errors.Is(err, ErrAbuseStaffRoleRefused) {
			t.Errorf("legal ran %s: %v", body, err)
		}
	}
	if 0 < len(acts.requests) {
		t.Fatalf("a refused request acted: %v", acts.kinds)
	}
	acts.err = sidecarmodel.ErrComputeAbuseHoldArgs
	if _, err := AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"compute_resume","network_id":"`+networkId+`"}`)); !errors.Is(err, ErrAbuseStaffRequestInvalid) {
		t.Errorf("an act's refusal of its arguments: %v", err)
	}
	acts.err = errors.New("the store did not answer")
	if _, err := AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"compute_resume","network_id":"`+networkId+`"}`)); err == nil || errors.Is(err, ErrAbuseStaffRequestInvalid) {
		t.Errorf("a store's failure: %v", err)
	}

	unlinked := installTestAbuseComputeHoldActs(t, false)
	if _, err := AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"compute_suspend","network_id":"`+networkId+`","reason":"spam"}`)); !errors.Is(err, ErrAbuseComputeHoldUnlinked) {
		t.Errorf("a binary without MG-C1: %v", err)
	}
	if 0 < len(unlinked.requests) {
		t.Fatal("a binary without MG-C1 acted")
	}
}
