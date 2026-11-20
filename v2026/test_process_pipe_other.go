//go:build !unix

// Unsupported descriptor inheritance retains a compiling, fail-closed boundary.
package server

// Refuse without acquiring a handle on platforms without Unix ExtraFiles.
func ValidateTestProcessPipe(uintptr, TestProcessPipeAccess) error {
	return errInvalidTestProcessPipe
}
