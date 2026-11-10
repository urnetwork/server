//go:build linux

// Workspace observations are immutable and cannot silently renew after change.
package competitionagent

import (
	"strings"
	"testing"
)

// Fresh maps model independent bounded kernel observations.
func workspaceGenerationFixture() *runtimeWorkspaceProof {
	return &runtimeWorkspaceProof{Invocation: strings.Repeat("a", 32),
		Parent: runtimeBoundaryMount{Id: 1, Options: map[string]bool{"rw": true}},
		Work:   runtimeBoundaryMount{Id: 2, SharedGroup: 27, Options: map[string]bool{"rw": true}},
		Peers: map[string]runtimeBoundaryMount{
			runtimeBoundaryEngineUnitName:     {Id: 3, MasterGroup: 27, Options: map[string]bool{"rw": true}},
			runtimeBoundaryContainerdUnitName: {Id: 3, MasterGroup: 27, Options: map[string]bool{"rw": true}},
		}}
}

// Restarted unit, replaced mount or changed propagation always invalidates.
func TestRuntimeWorkspaceProofNeverRenewsChangedGeneration(t *testing.T) {
	for _, change := range []func(*runtimeWorkspaceProof){
		func(p *runtimeWorkspaceProof) { p.Invocation = strings.Repeat("b", 32) },
		func(p *runtimeWorkspaceProof) { p.Parent.Id++ }, func(p *runtimeWorkspaceProof) { p.Work.Id++ }, func(p *runtimeWorkspaceProof) { p.Work.SharedGroup++ },
		func(p *runtimeWorkspaceProof) {
			mount := p.Peers[runtimeBoundaryEngineUnitName]
			mount.Id++
			p.Peers[runtimeBoundaryEngineUnitName] = mount
		},
	} {
		owner := &runtimeBoundaryNativeNamespace{}
		if err := owner.acceptWorkspaceProof(workspaceGenerationFixture()); err != nil {
			t.Fatal(err)
		}
		changed := workspaceGenerationFixture()
		change(changed)
		if err := owner.acceptWorkspaceProof(changed); err == nil {
			t.Fatal("changed generation renewed the workspace lease")
		}
		if err := owner.acceptWorkspaceProof(workspaceGenerationFixture()); err != nil {
			t.Fatal("rejection mutated original proof", err)
		}
	}
}

// Caller map mutation must not rewrite the retained generation implicitly.
func TestRuntimeWorkspaceProofOwnsObservedMaps(t *testing.T) {
	owner := &runtimeBoundaryNativeNamespace{}
	input := workspaceGenerationFixture()
	if err := owner.acceptWorkspaceProof(input); err != nil {
		t.Fatal(err)
	}
	input.Work.Options["rw"] = false
	input.Parent.Options["rw"] = false
	input.Peers[runtimeBoundaryEngineUnitName].Options["rw"] = false
	if err := owner.acceptWorkspaceProof(workspaceGenerationFixture()); err != nil {
		t.Fatal("caller rewrote retained observation", err)
	}
}

// Actual close completes before a replacement observation is offered.
func TestRuntimeWorkspaceProofCannotCaptureAfterClose(t *testing.T) {
	owner := &runtimeBoundaryNativeNamespace{}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.acceptWorkspaceProof(workspaceGenerationFixture()); err == nil || owner.workspace != nil {
		t.Fatal("closed receipt captured a new generation")
	}
}
