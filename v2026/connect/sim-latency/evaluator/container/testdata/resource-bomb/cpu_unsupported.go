//go:build !linux

// Unsupported hosts retain parser coverage while refusing to pretend that the
// Linux-only affinity resource bomb can run there.
package main

import "fmt"

// Reject affinity setup on kernels that do not expose the Linux contract used
// by the production evaluator container.
func pinCurrentThreadToCPU(cpu int) error {
	return fmt.Errorf("pin CPU %d: resource bomb requires Linux", cpu)
}
