//go:build !linux

package controller

import "errors"

// No pathname-based fallback can widen the Linux-only housekeeping authority.
func captureCompetitionReceiveNative(root, directory, runId string) (competitionReceiveNativeCustody, error) {
	return nil, errors.New("receive cleanup requires native Linux descriptor/mount custody")
}
