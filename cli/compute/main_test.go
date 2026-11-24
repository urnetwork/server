package main

import (
	"testing"

	"github.com/urnetwork/server/sidecarhost"
)

// The block's main scrubs its logs before anything can log, as every sidecar block's does.
func TestComputeMainScrubsLogsFirst(t *testing.T) {
	if err := sidecarhost.CheckMainScrubsLogsFirst("main.go"); err != nil {
		t.Fatal(err)
	}
}
