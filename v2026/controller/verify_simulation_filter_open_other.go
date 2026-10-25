//go:build !unix

package controller

import (
	"errors"
	"os"
)

// The simulator is deployed on Unix, where the descriptor-relative no-follow
// implementation is available. Other targets fail closed if the simulator
// filter is configured rather than falling back to a racy pathname check.
func verifySimulationOpenAssignmentFilterNoFollow(path string) (*os.File, bool, error) {
	return nil, false, errors.New("secure verify simulation assignment filters are unsupported on this platform")
}
