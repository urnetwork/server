// Proxy client-host documentation tests retain local-versus-remote incident
// discriminators that are collected only by the acceptance runner.
package monitor

import (
	"os"
	"strings"
	"testing"
)

// Keeps both local Darwin failure shapes, their negatives, and the no-retry
// recovery contract together in the public protocol runbook.
func TestProxyClientHostFailureDocumentationContract(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	start := strings.Index(catalog, "**Local Darwin buffer-pressure and Wi-Fi-stall signatures (2026-09-05):**")
	if start < 0 {
		t.Fatal("SIGNALS.md §14.5 is missing the local Darwin failure boundary")
	}
	section := catalog[start:]
	end := strings.Index(section, "\nThe proxy service intentionally")
	if end < 0 {
		t.Fatal("SIGNALS.md §14.5 is missing the end of the local Darwin failure boundary")
	}
	section = strings.Join(strings.Fields(section[:end]), " ")

	for _, required := range []string{
		"`write: socket is not connected`",
		"`sendmsg: no buffer space available`",
		"`skmem_slab_alloc_locked ... failed to allocate slab`",
		"`netif_gso_tcp_segment_mbuf failed to alloc`",
		"`phase connecting_tunnel_failed`",
		"`DPS Symptoms` with `StallScore:50`",
		"cannot cause a request that never reached `GotConn`",
		"`local_host{...}`",
		"`local-kernel-buffer-pressure`",
		"`local-wifi-stall`",
		"`no-local-kernel-signal`",
		"`query-unavailable`",
		"Do not retry, lengthen the timeout, or convert either signature to PASS.",
		"`tests/network-intensive-suite-lock.sh`",
		"cannot control arbitrary user or WLAN load",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("proxy client-host runbook missing %q", required)
		}
	}
}
