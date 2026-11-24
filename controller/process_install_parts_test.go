package controller

// InstallProcessParts installs the lists of a process's services as one: each name once, the first in list order, a
// covered part not at all wherever its cover stands in the lists, and the undo removes what it installed in the
// reverse order, one removal at a time. A list whose cover is itself covered is refused, since its cover would be
// left out with what it covers. cli/all's tests hold the real lists to this.

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/sidecar"
)

// A context value the parts check they were installed on.
type testProcessInstallContextKey struct{}

// Parts that write each install and removal to log, as "+name" and "-name", where name is the part's and tag's.
type testProcessInstallLog struct {
	t       *testing.T
	ctx     context.Context
	entries []string
}

func (self *testProcessInstallLog) part(name ProcessInstallPartName, tag string, covers ...ProcessInstallPartName) ProcessInstallPart {
	return ProcessInstallPart{
		Name:   name,
		Covers: covers,
		Install: func(ctx context.Context) func() {
			if ctx.Value(testProcessInstallContextKey{}) != self.ctx.Value(testProcessInstallContextKey{}) {
				self.t.Errorf("%s%s was installed on another context", name, tag)
			}
			self.entries = append(self.entries, "+"+string(name)+tag)
			return func() {
				self.entries = append(self.entries, "-"+string(name)+tag)
			}
		},
	}
}

// Each name is installed once, the first of it in list order, and the undo removes them in the reverse.
func TestInstallProcessPartsInstallsEachNameOnceAndRemovesInReverse(t *testing.T) {
	ctx := context.WithValue(t.Context(), testProcessInstallContextKey{}, "installs")
	log := &testProcessInstallLog{t: t, ctx: ctx}
	// two services' lists, as one process installs them: b and a named by both
	first := []ProcessInstallPart{log.part("a", "1"), log.part("b", "1"), log.part("c", "1")}
	second := []ProcessInstallPart{log.part("b", "2"), log.part("d", "2"), log.part("a", "2")}
	undo := InstallProcessParts(ctx, slices.Concat(first, second))
	if want := []string{"+a1", "+b1", "+c1", "+d2"}; !slices.Equal(log.entries, want) {
		t.Fatalf("installed %v, want %v", log.entries, want)
	}
	undo()
	if want := []string{"+a1", "+b1", "+c1", "+d2", "-d2", "-c1", "-b1", "-a1"}; !slices.Equal(log.entries, want) {
		t.Fatalf("installed and removed %v, want %v", log.entries, want)
	}
}

// A covered part is not installed, whether it stands before its cover in the lists or after it, and a part covers
// what it names whether or not it is the first of its name.
func TestInstallProcessPartsLeavesCoveredPartsOut(t *testing.T) {
	ctx := context.WithValue(t.Context(), testProcessInstallContextKey{}, "covers")
	log := &testProcessInstallLog{t: t, ctx: ctx}
	parts := []ProcessInstallPart{
		// covered by a part later in the lists
		log.part("hard-deny", "1"),
		log.part("standing", "1", "hard-deny"),
		log.part("authorization", "1"),
		// the second of its name, whose cover still holds
		log.part("standing", "2", "gate"),
		// covered by a part earlier in the lists
		log.part("hard-deny", "2"),
		log.part("gate", "2"),
	}
	selected := SelectProcessInstallParts(parts)
	selectedNames := []ProcessInstallPartName{}
	for _, part := range selected {
		selectedNames = append(selectedNames, part.Name)
	}
	if want := []ProcessInstallPartName{"standing", "authorization"}; !slices.Equal(selectedNames, want) {
		t.Fatalf("selected %v, want %v", selectedNames, want)
	}
	undo := InstallProcessParts(ctx, parts)
	undo()
	if want := []string{"+standing1", "+authorization1", "-authorization1", "-standing1"}; !slices.Equal(log.entries, want) {
		t.Fatalf("installed and removed %v, want %v", log.entries, want)
	}
}

// A list whose cover is itself covered is refused before anything is installed.
func TestInstallProcessPartsRefusesACoverThatIsCovered(t *testing.T) {
	ctx := context.WithValue(t.Context(), testProcessInstallContextKey{}, "refused")
	log := &testProcessInstallLog{t: t, ctx: ctx}
	parts := []ProcessInstallPart{
		log.part("a", "", "b"),
		log.part("b", "", "c"),
		log.part("c", ""),
	}
	refused := func() (refused bool) {
		defer func() {
			refused = recover() != nil
		}()
		InstallProcessParts(ctx, parts)
		return false
	}()
	if !refused {
		t.Fatal("a list whose cover is covered was installed")
	}
	if len(log.entries) != 0 {
		t.Fatalf("a refused list installed %v", log.entries)
	}
}

// Each service's own list installs whole in a process that runs that service alone: no name twice and no part its
// own list covers, so a standalone main installs every part it names. §21's parts lead api's list, standing before
// certs, whose gate reads the hard deny standing installs.
func TestApiAndConnectListsInstallWholeInTheirOwnProcess(t *testing.T) {
	// no part is installed, so the host is never read
	host := &sidecar.ServiceHost{}
	for name, parts := range map[string][]ProcessInstallPart{
		"api":     ApiRoutesInstallParts(host, nil),
		"connect": ConnectExchangeInstallParts(host),
		"abuse":   AbuseInstallParts(),
	} {
		if selected := SelectProcessInstallParts(parts); len(selected) != len(parts) {
			t.Errorf("%s's list installs %d of its %d parts alone", name, len(selected), len(parts))
		}
	}
	apiNames := []ProcessInstallPartName{}
	for _, part := range ApiRoutesInstallParts(host, nil) {
		apiNames = append(apiNames, part.Name)
	}
	want := []ProcessInstallPartName{
		InstallPartAbuseEffects,
		InstallPartAbuseRecordMailer,
		InstallPartLegalHoldRecordMailer,
		InstallPartAbuseNoticePager,
		InstallPartAbuseStanding,
		InstallPartLegalHoldDispatch,
		InstallPartApiAuthorization,
		InstallPartDeviceAuthorizationStore,
		InstallPartTlsCerts,
	}
	if !slices.Equal(apiNames, want) {
		t.Fatalf("api's parts %v, want %v", apiNames, want)
	}
}
