// Synthetic complete boundaries pin the historical whole-value encoding and
// prove that prospective identity never supplies native admission authority.
package competitionagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The literal old encoder, retained only in this regression, independently
// detects a field/order/omission change in the shared private encoder.
func TestProspectiveNativeIdentityPreservesHistoricalBoundaryBytes(t *testing.T) {
	fixture := newRuntimeNativeClassTest(t)
	boundary := fixture.boundary
	selection := *boundary.NativeClass
	selection.Receipt = Pin{}
	boundary.NativeClass = &selection
	content, err := json.Marshal(boundary)
	if err != nil {
		t.Fatal(err)
	}
	expected := digestDockerBytes(content)
	actual, err := RuntimeNativeClassIdentity(fixture.boundary)
	if err != nil || actual != expected {
		t.Fatal("public admission metadata encoding changed", actual, expected, err)
	}
	prospective, err := prospectiveNativeQualificationIdentity(fixture.boundary)
	if err != nil || prospective != expected {
		t.Fatal("prospective identity invented another canonical boundary", prospective, err)
	}
	for _, receipt := range []Pin{{}, {Path: "/etc/synthetic-private-engine/future.json"}, {Sha256: strings.Repeat("1", 64)}} {
		copy := *fixture.boundary.NativeClass
		copy.Receipt = receipt
		pending := fixture.boundary
		pending.NativeClass = &copy
		got, err := prospectiveNativeQualificationIdentity(pending)
		if err != nil || got != expected {
			t.Fatal("prospective calculation required a fabricated future receipt", got, err)
		}
	}
}

// Missing acceptance still refuses both old public metadata/admission paths;
// computing a prospective digest cannot install a proof or verify the image.
func TestProspectiveNativeIdentityNeverAdmitsMissingReceipt(t *testing.T) {
	fixture := newRuntimeNativeClassTest(t)
	fixture.boundary.NativeClass.Receipt = Pin{}
	if _, err := prospectiveNativeQualificationIdentity(fixture.boundary); err != nil {
		t.Fatal("complete prospective release refused", err)
	}
	if fixture.boundary.NativeClass.validate(fixture.boundary) == nil {
		t.Fatal("prospective release bypassed required receipt pin")
	}
	if _, err := runtimeNativeIdentity(fixture.boundary); err == nil {
		t.Fatal("private admission metadata accepted missing receipt")
	}
	if _, err := RuntimeNativeClassIdentity(fixture.boundary); err == nil {
		t.Fatal("public admission metadata accepted missing receipt")
	}
	if err := prepareRuntimeNativeClass(context.Background(), &fixture.boundary, fixture.scope, fixture.settings, fixture.host); err == nil || fixture.boundary.nativeClass != nil {
		t.Fatal("prospective identity fabricated admitted proof", err)
	}
	if len(fixture.host.events) != 0 {
		t.Fatal("missing acceptance reached native host access")
	}
}

// The new preparation seam cannot erase any release, profile, helper or
// candidate-storage prohibition merely because the receipt is intentionally nil.
func TestProspectiveNativeIdentityRefusesPartialOrUnsafeRelease(t *testing.T) {
	for _, mutate := range []func(*RuntimeResourceBoundary){
		func(value *RuntimeResourceBoundary) { value.NativeClass = nil },
		func(value *RuntimeResourceBoundary) { value.NativeClass.Profile = "unsupported-synthetic-profile" },
		func(value *RuntimeResourceBoundary) { value.NativeClass.Release.HostId = "" },
		func(value *RuntimeResourceBoundary) { value.NativeClass.Release.SourceLockSha256 = "" },
		func(value *RuntimeResourceBoundary) { value.NativeClass.Release.EvaluatorImageDigest = "" },
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.Agent.Path = value.CandidateRoot + "/agent"
		},
		func(value *RuntimeResourceBoundary) {
			value.NativeClass.Release.Assets = value.NativeClass.Release.Assets[:1]
		},
		func(value *RuntimeResourceBoundary) { value.NativeClass.Release.Assets[0].Name = "unknown-helper" },
	} {
		fixture := newRuntimeNativeClassTest(t)
		fixture.boundary.NativeClass.Receipt = Pin{}
		mutate(&fixture.boundary)
		if _, err := prospectiveNativeQualificationIdentity(fixture.boundary); err == nil {
			t.Fatal("partial or unsafe prospective release acquired identity")
		}
	}
}
