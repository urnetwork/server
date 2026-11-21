// The sockets block's main: it scrubs its logs before anything can log, and reads api's base URL from the block's
// environment. What the host does is socketshost's tests.

package main

import (
	"flag"
	"testing"

	"github.com/urnetwork/server/sidecarhost"
)

// The block's main scrubs its logs before anything can log, as every sidecar block's does.
func TestMainScrubsLogsFirst(t *testing.T) {
	if err := sidecarhost.CheckMainScrubsLogsFirst("main.go"); err != nil {
		t.Fatal(err)
	}
}

// A deployed block is given api's base URL in its environment, $UR_API_URL, from sockets.yml's env_vars, which -api_url
// reads as its default (sidecarhost.ServiceUrlFlag).
func TestApiUrlFlagDefaultsToTheBlocksEnvironment(t *testing.T) {
	if err := sidecarhost.CheckServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sidecarhost.ApiUrlEnvName, "https://api.example.com")
	if apiUrl.Url() != "https://api.example.com" {
		t.Fatalf("-api_url reads %q", apiUrl.Url())
	}
}
