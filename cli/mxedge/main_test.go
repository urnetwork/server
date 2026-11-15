package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server/sidecarhost"
)

// The block's main scrubs its logs before anything can log, as every sidecar block's does.
func TestMainScrubsLogsFirst(t *testing.T) {
	if err := sidecarhost.CheckMainScrubsLogsFirst("main.go"); err != nil {
		t.Fatal(err)
	}
}

// Each environment's mxedge.yml, in the config checkout beside this one, runs both roles: the router's
// route_root and the platform's three names are there, and every key is one the engine knows.
func TestEachMxEdgeYmlRunsBothRoles(t *testing.T) {
	for _, env := range []string{"main", "local"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", env, "mxedge.yml"))
		if err != nil {
			t.Fatal(err)
		}
		settings, err := mxedgeservice.LoadMxEdgeSettings(data)
		if err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		if err := settings.ValidateRouter(); err != nil {
			t.Errorf("%s: the router cannot run: %v", env, err)
		}
		if err := settings.ValidatePlatform(); err != nil {
			t.Errorf("%s: the platform listener cannot run: %v", env, err)
		}
	}
}
