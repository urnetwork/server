//go:build linux

// The Linux affinity boundary pins and verifies each hostile CPU worker before
// the fixture tells the evaluator that the resource bomb is ready.
package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Pin the current locked thread and verify the kernel scheduled it on the
// requested CPU before returning.
func pinCurrentThreadToCPU(cpu int) error {
	var affinity unix.CPUSet
	affinity.Set(cpu)
	if !affinity.IsSet(cpu) {
		return fmt.Errorf("CPU %d exceeds the fixed affinity set", cpu)
	}
	if err := unix.SchedSetaffinity(0, &affinity); err != nil {
		return fmt.Errorf("pin CPU %d: %w", cpu, err)
	}
	var currentCPU uint32
	_, _, errno := unix.RawSyscall(
		unix.SYS_GETCPU,
		uintptr(unsafe.Pointer(&currentCPU)),
		0,
		0,
	)
	if errno != 0 {
		return fmt.Errorf("read current CPU for %d: %w", cpu, errno)
	}
	if int(currentCPU) != cpu {
		return fmt.Errorf("worker for CPU %d executed on CPU %d", cpu, currentCPU)
	}
	return nil
}
