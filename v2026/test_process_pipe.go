// Shared test-process preflight checks descriptors without taking ownership.
package server

import "errors"

// Each inherited endpoint must have exactly its caller-declared direction.
type TestProcessPipeAccess uint8

const (
	TestProcessPipeRead TestProcessPipeAccess = iota + 1
	TestProcessPipeWrite
)

var errInvalidTestProcessPipe = errors.New("test process descriptor is not the expected pipe")
