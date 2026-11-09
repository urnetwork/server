package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

// §12.5's release check (DNS-M0-01): services.yml's host labels against
// sidecar's reserved list. The fixtures are synthetic: no real host, address or
// hidden prefix.

// A services.yml whose every host label is reserved, in the shape warp reads,
// with hidden prefixes the check leaves out.
const testReleaseServicesYaml = `
domain: bringyour.example
domains:
    bringyour.example: route53
expose_aliases:
    - ur.example
hidden_prefixes:
    - synthetic-hidden-prefix
routers:
    by-xx-site-1-gateway-0:
        class: gateway
versions:
-   lb:
        interfaces:
            by-xx-site-1-edge-0.bringyour.example:
                en0:
                    ipv4: 192.0.2.10
    services:
        api:
            expose_aliases:
                - api.bringyour.example
        connect:
            redirect_aliases:
                connect.ur.example: https://ur.example
        alt:
            exposed: no
            dns_aliases:
                - alt-v4.bringyour.example
        web:
            expose_domains:
                - recurd.example
`

// Every label the active version gives, each once, sorted, and no hidden
// prefix; an unexposed service has no <env>-<service> host.
func TestReleaseServicesHostLabels(t *testing.T) {
	labels, err := releaseServicesHostLabels([]byte(testReleaseServicesYaml), "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"alt", "alt-v4", "api", "bringyour", "by-xx-site-1-edge-0", "by-xx-site-1-gateway-0", "connect",
		"main-api", "main-connect", "main-lb", "main-web", "recurd", "ur", "web",
	}
	if !slices.Equal(labels, want) {
		t.Fatalf("labels %v, want %v", labels, want)
	}
	if _, err := releaseServicesHostLabels([]byte("domain: x.example\n"), "main"); err == nil {
		t.Fatal("read a services.yml with no version")
	}
	if _, err := releaseServicesHostLabels([]byte(testReleaseServicesYaml), ""); err == nil {
		t.Fatal("read the hosts with no env")
	}
}

// A services.yml whose host labels are all reserved passes; one with a service,
// an alias or an lb host the list lacks fails, naming exactly those.
func TestReleasePreflightRefusesAnUnlistedHostLabel(t *testing.T) {
	var out bytes.Buffer
	if err := checkReleaseDnsReservedLabels([]byte(testReleaseServicesYaml), "main", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "14 services.yml host labels are reserved") {
		t.Fatalf("printed %q", out.String())
	}

	unlisted := strings.Replace(testReleaseServicesYaml, "        web:\n", "        newsvc:\n            exposed: no\n        web:\n", 1)
	unlisted = strings.Replace(unlisted, "    - ur.example\n", "    - ur.example\n    - shiny.bringyour.example\n", 1)
	unlisted = strings.Replace(unlisted, "            by-xx-site-1-edge-0.bringyour.example:\n", "            edgehost.bringyour.example:\n                en0:\n                    ipv4: 192.0.2.11\n            by-xx-site-1-edge-0.bringyour.example:\n", 1)
	err := checkReleaseDnsReservedLabels([]byte(unlisted), "main", &out)
	if err == nil {
		t.Fatal("passed a services.yml with unlisted host labels")
	}
	if !strings.HasSuffix(err.Error(), ": edgehost, newsvc, shiny") {
		t.Fatalf("refused with %q", err)
	}

	// an env no pattern covers names its lb and service hosts by itself
	if err := checkReleaseDnsReservedLabels([]byte(testReleaseServicesYaml), "staging", &out); err == nil || !strings.Contains(err.Error(), "staging-api, staging-connect, staging-web") {
		t.Fatalf("an unlisted env answered %v", err)
	}
}
