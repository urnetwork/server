// Injected native host facts exercise concrete capture/check logic without
// reading procfs, running an executable, or contacting a Docker daemon.
package competitionagent

import (
	"context"
	"errors"
	"testing"
)

// Exact mutation counters force both snapshot and bounded-read boundaries.
type nativeQualificationReleaseTestHost struct {
	*runtimeNativeClassTestHost
	kernelReads     int
	imageReads      int
	documentReads   int
	imageLimits     []int64
	mutateKernel    bool
	mutateImage     bool
	wrongImage      bool
	onBuild         bool
	documentError   error
	documentDrift   bool
	executableError error
	cancel          context.CancelFunc
}

// Only a requested second snapshot changes; no executing-host facts are used.
func (self *nativeQualificationReleaseTestHost) NativeClassKernel(ctx context.Context, settings RuntimeResourceBoundarySettings) (RuntimeNativeKernelIdentity, error) {
	self.kernelReads++
	value, err := self.runtimeNativeClassTestHost.NativeClassKernel(ctx, settings)
	if self.mutateKernel && self.kernelReads == 2 {
		value.CorePipeLimit++
	}
	return value, err
}

// Retain the actual read ceiling delivered to the host, not just an after-read
// length check. Changed whitespace still identifies a different raw snapshot.
func (self *nativeQualificationReleaseTestHost) NativeClassImage(ctx context.Context, boundary RuntimeResourceBoundary, image string, settings RuntimeResourceBoundarySettings) ([]byte, error) {
	self.imageReads++
	self.imageLimits = append(self.imageLimits, settings.NativeBytes)
	data, err := self.runtimeNativeClassTestHost.NativeClassImage(ctx, boundary, image, settings)
	if self.wrongImage {
		data = []byte(`{"Id":"sha256:synthetic-mismatch","Os":"linux","Architecture":"amd64"}`)
	}
	if self.onBuild {
		data = []byte(`{"Id":"` + image + `","Os":"linux","Architecture":"amd64","Config":{"OnBuild":["synthetic forbidden hook"]}}`)
	}
	if self.mutateImage && self.imageReads == 2 {
		data = append(data, ' ')
	}
	if self.cancel != nil {
		self.cancel()
	}
	return data, err
}

// The captured document hash is checked by the real capture function.
func (self *nativeQualificationReleaseTestHost) Document(pin Pin, maximum int64) ([]byte, error) {
	self.documentReads++
	if self.documentError != nil {
		return nil, self.documentError
	}
	data, err := self.runtimeNativeClassTestHost.Document(pin, maximum)
	if self.documentDrift && self.documentReads > len(runtimeNativeAssetNames())+2 {
		data = append(append([]byte(nil), data...), ' ')
	}
	return data, err
}

// No actual executable is run by this fixture.
func (self *nativeQualificationReleaseTestHost) Executable(ctx context.Context, pin Pin, maximum int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if maximum <= 0 {
		return 0, errors.New("synthetic executable bound missing")
	}
	if self.executableError != nil {
		return 0, self.executableError
	}
	return 1, self.runtimeNativeClassTestHost.Executable(pin)
}

// Two actual-operation snapshots and exact profiles are required, but these
// intermediate readbacks still do not create a native proof on the boundary.
func TestNativeQualificationKernelReleaseBindsBothConcreteSnapshots(t *testing.T) {
	fixture := newRuntimeNativeClassTest(t)
	fixture.boundary.NativeClass.Receipt = Pin{}
	host := &nativeQualificationReleaseTestHost{runtimeNativeClassTestHost: fixture.host}
	facts, err := captureNativeQualificationKernelRelease(context.Background(), fixture.boundary, fixture.settings, DefaultCandidateCoreProfileSettings(), host)
	if err != nil || host.kernelReads != 2 || host.imageReads != 2 || !facts.ProfilesDerivedExactly || len(facts.DocumentPins) != len(runtimeNativeAssetNames())+2 || len(facts.ExecutablePins) != 6 || facts.KernelBefore != facts.KernelAfter || fixture.boundary.nativeClass != nil {
		t.Fatal("exact release observation missing or admitted authority", facts, err)
	}
}

// Image hooks/platform identity, raw metadata mutation, executable failure and
// document drift are independent refusal paths with original errors retained.
func TestNativeQualificationKernelReleaseRefusesDriftAndUnsafeImages(t *testing.T) {
	sentinel := errors.New("synthetic protected release read failure")
	for _, fault := range []string{"kernel", "image", "identity", "hook", "document", "document-drift", "profile", "executable"} {
		fixture := newRuntimeNativeClassTest(t)
		fixture.boundary.NativeClass.Receipt = Pin{}
		host := &nativeQualificationReleaseTestHost{runtimeNativeClassTestHost: fixture.host}
		switch fault {
		case "kernel":
			host.mutateKernel = true
		case "image":
			host.mutateImage = true
		case "identity":
			host.wrongImage = true
		case "hook":
			host.onBuild = true
		case "document":
			host.documentError = sentinel
		case "document-drift":
			host.documentDrift = true
		case "profile":
			fixture.boundary.NativeClass.Release.CandidateCoreProfile = fixture.document("synthetic-wrong-core.json", []byte("synthetic wrong but pinned profile"))
		case "executable":
			host.executableError = sentinel
		}
		_, err := captureNativeQualificationKernelRelease(context.Background(), fixture.boundary, fixture.settings, DefaultCandidateCoreProfileSettings(), host)
		if err == nil || (fault == "document" || fault == "executable") && !errors.Is(err, sentinel) {
			t.Fatal("changed release was accepted or error erased", fault, err)
		}
	}
}

// The exact aggregate edge succeeds; one fewer byte fails at the host boundary
// and both image read calls receive their remaining allowance before reading.
func TestNativeQualificationKernelReleaseEnforcesAggregateAndCancellation(t *testing.T) {
	fixture := newRuntimeNativeClassTest(t)
	fixture.boundary.NativeClass.Receipt = Pin{}
	before, cancelBefore := context.WithCancel(context.Background())
	cancelBefore()
	untouched := &nativeQualificationReleaseTestHost{runtimeNativeClassTestHost: fixture.host}
	if _, err := captureNativeQualificationKernelRelease(before, fixture.boundary, fixture.settings, DefaultCandidateCoreProfileSettings(), untouched); !errors.Is(err, context.Canceled) || untouched.kernelReads != 0 || untouched.imageReads != 0 || untouched.documentReads != 0 {
		t.Fatal("pre-canceled capture dispatched native IO", err)
	}
	baseline, err := captureNativeQualificationKernelRelease(context.Background(), fixture.boundary, fixture.settings, DefaultCandidateCoreProfileSettings(), &nativeQualificationReleaseTestHost{runtimeNativeClassTestHost: fixture.host})
	if err != nil {
		t.Fatal(err)
	}
	settings := fixture.settings
	settings.NativeBytes = baseline.MetadataBytesRead
	host := &nativeQualificationReleaseTestHost{runtimeNativeClassTestHost: fixture.host}
	actual, err := captureNativeQualificationKernelRelease(context.Background(), fixture.boundary, settings, DefaultCandidateCoreProfileSettings(), host)
	if err != nil || actual.MetadataBytesRead != settings.NativeBytes || len(host.imageLimits) != 2 || host.imageLimits[1] >= host.imageLimits[0] {
		t.Fatal("remaining image budget was not enforced before reading", actual, host.imageLimits, err)
	}
	settings.NativeBytes--
	if _, err := captureNativeQualificationKernelRelease(context.Background(), fixture.boundary, settings, DefaultCandidateCoreProfileSettings(), &nativeQualificationReleaseTestHost{runtimeNativeClassTestHost: fixture.host}); err == nil {
		t.Fatal("one byte below aggregate allowance passed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	host = &nativeQualificationReleaseTestHost{runtimeNativeClassTestHost: fixture.host, cancel: cancel}
	if _, err := captureNativeQualificationKernelRelease(ctx, fixture.boundary, fixture.settings, DefaultCandidateCoreProfileSettings(), host); !errors.Is(err, context.Canceled) || host.documentReads != 0 {
		t.Fatal("parent cancellation reached later release IO", err, host.documentReads)
	}
	profileSettings := DefaultCandidateCoreProfileSettings()
	profileSettings.DocumentBytes = 1
	if _, err := captureNativeQualificationKernelRelease(context.Background(), fixture.boundary, fixture.settings, profileSettings, &nativeQualificationReleaseTestHost{runtimeNativeClassTestHost: fixture.host}); err == nil {
		t.Fatal("capture replaced caller's finite profile policy with global defaults")
	}
}
