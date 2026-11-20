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

// A deployed block is given the dns API block's base URL in its environment, $UR_API_URL, from dnsedge.yml's env_vars,
// which -api_url reads as its default (sidecarhost.ServiceUrlFlag).
func TestApiUrlFlagDefaultsToTheBlocksEnvironment(t *testing.T) {
	if err := sidecarhost.CheckServiceUrlFlag(flag.CommandLine, "api_url", sidecarhost.ApiUrlEnvName); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sidecarhost.ApiUrlEnvName, "https://dns.example.com")
	if apiUrl.Url() != "https://dns.example.com" {
		t.Fatalf("-api_url reads %q", apiUrl.Url())
	}
}
