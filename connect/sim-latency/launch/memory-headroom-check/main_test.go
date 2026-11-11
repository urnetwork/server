// Synthetic byte budgets exercise the guard without allocating host memory.
package main

import (
	"strings"
	"testing"
)

// Equality passes; one more required byte refuses without starting any work.
func TestHeadroomInclusiveBoundaryAndRefusal(t *testing.T) {
	config := defaultSettings()
	config.minimumAvailableBytes = 2 * 1024
	content := []byte("MemTotal: 8 kB\nMemAvailable: 2 kB\n")
	available, passed, err := checkHeadroom(content, config)
	if err != nil || !passed || available != config.minimumAvailableBytes {
		t.Fatal("exact boundary refused", available, passed, err)
	}
	config.minimumAvailableBytes++
	if _, passed, err := checkHeadroom(content, config); err != nil || passed {
		t.Fatal("insufficient headroom admitted", passed, err)
	}
}

// Missing, duplicate, negative, noncanonical and overflowing fields refuse.
func TestHeadroomRejectsAmbiguousKernelFields(t *testing.T) {
	for _, content := range []string{
		"MemFree: 2 kB\n", "MemAvailable: 1 kB\nMemAvailable: 1 kB\n", "MemAvailable: -1 kB\n",
		"MemAvailable: +1 kB\n", "MemAvailable: 01 kB\n", "MemAvailable: 1 KB\n", "MemAvailable: 1 kB extra\n",
		"MemAvailable: 9223372036854775807 kB\n",
	} {
		if _, err := availableMemoryBytes([]byte(content)); err == nil {
			t.Fatal("malformed memory facts accepted", content)
		}
	}
}

// Bounds and nonpositive settings cannot silently disable the prerequisite.
func TestHeadroomRequiresBoundedPositiveOwner(t *testing.T) {
	config := defaultSettings()
	if _, passed, err := checkHeadroom([]byte("MemAvailable: 1 kB\n"), config); err == nil || passed {
		t.Fatal("missing requirement accepted")
	}
	config.minimumAvailableBytes = 1
	if _, passed, err := checkHeadroom([]byte(strings.Repeat("x", int(config.maximumMeminfoBytes)+1)), config); err == nil || passed {
		t.Fatal("oversized kernel document accepted")
	}
	config.maximumMeminfoBytes = 0
	if _, passed, err := checkHeadroom(nil, config); err == nil || passed {
		t.Fatal("unbounded reader accepted")
	}
}
