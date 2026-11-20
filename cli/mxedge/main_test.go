package main

import (
	"flag"
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

// A deployed block is given api's base URL in its environment, $UR_API_URL, from mxedge.yml and mxedge-platform.yml's
// env_vars, which -api_url reads as its default (sidecarhost.ServiceUrlFlag).
func TestApiUrlFlagDefaultsToTheBlocksEnvironment(t *testing.T) {
	if err := sidecarhost.CheckServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sidecarhost.ApiUrlEnvName, "https://api.example.com")
	if apiUrl.Url() != "https://api.example.com" {
		t.Fatalf("-api_url reads %q", apiUrl.Url())
	}
}

// A deployed block is given the sockets service's base URL in its environment, $UR_SOCKETS_URL, from mxedge.yml's
// env_vars, which -sockets_url reads as its default (sidecarhost.ServiceUrlFlag).
func TestSocketsUrlFlagDefaultsToTheBlocksEnvironment(t *testing.T) {
	if err := sidecarhost.CheckServiceUrlFlag(flag.CommandLine, "sockets_url", sidecarhost.SocketsUrlEnvName); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sidecarhost.SocketsUrlEnvName, "https://sockets.example.com")
	if socketsUrl.Url() != "https://sockets.example.com" {
		t.Fatalf("-sockets_url reads %q", socketsUrl.Url())
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
