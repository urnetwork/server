//go:build !linux

// Unsupported-platform coverage keeps host-side test discovery useful without
// weakening the fixture's Linux affinity requirement.
package main

import "testing"

// Confirm a non-Linux host fails before advertising a pinned CPU worker.
func TestPinCurrentThreadToCPURejectsUnsupportedPlatform(t *testing.T) {
	if err := pinCurrentThreadToCPU(0); err == nil {
		t.Fatal("non-Linux CPU affinity was accepted")
	}
}
