//go:build linux

// Active network proofs retain the original runtime generation, not idle facts.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// Return only the exact finite ip query families used by the native reader.
type networkLifetimeNamespace struct {
	runtimeBoundaryTestNamespace
	bridge    string
	addresses []any
}

func (self *networkLifetimeNamespace) Run(ctx context.Context, _ Pin, arguments []string) ([]byte, error) {
	if err := self.Recheck(ctx); err != nil {
		return nil, err
	}
	query := strings.Join(arguments, " ")
	switch query {
	case "-j -d addr show":
		return json.Marshal([]map[string]any{
			{"ifindex": 1, "ifname": "lo", "link_type": "loopback", "addr_info": []any{}},
			{"ifindex": 2, "ifname": self.bridge, "link_type": "ether", "linkinfo": map[string]string{"info_kind": "bridge"}, "addr_info": self.addresses},
		})
	case "-j -4 route show table all", "-j -6 route show table all":
		return []byte("[]"), nil
	case "-j addr show dev " + self.bridge:
		return json.Marshal([]map[string]any{{"ifname": self.bridge, "addr_info": self.addresses}})
	default:
		return nil, errors.New("unexpected synthetic native query")
	}
}

// The fake daemon serves a real owned-network document; all native queries
// still pass the production network inventory and bridge validators.
func networkLifetimeFixture(t *testing.T) (*dockerProxyTest, *networkLifetimeNamespace, string) {
	t.Helper()
	fixture := newDockerProxyTest(t)
	fixture.proxy.plan.HostFiles = &HostFilesPlan{WorkDeviceId: 123}
	namespace := &networkLifetimeNamespace{bridge: "unev-" + fixture.plan.RunId[:10], addresses: []any{}}
	boundary := &RuntimeResourceBoundary{BaseDirectory: fixture.plan.BaseDirectory, WorkRoot: filepath.Dir(fixture.plan.ContextRoot),
		ScratchRoot: filepath.Dir(fixture.plan.ScratchDirectory), PrivateSocketPath: fixture.plan.UpstreamSocketPath, WorkDeviceId: 123, namespace: namespace}
	if err := fixture.proxy.bindRuntimeBoundary(boundary); err != nil {
		t.Fatal(err)
	}
	networkId := strings.Repeat("d", 64)
	fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/networks/"+networkId) {
			http.Error(w, "unexpected synthetic network request", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": networkId, "Name": "synthetic-owned-network", "Driver": "bridge", "Scope": "local", "Internal": true, "EnableIPv6": false, "Attachable": false, "Ingress": false,
			"Labels":  map[string]string{cleanupJobLabel: fixture.plan.JobId, cleanupRoundLabel: fixture.plan.RoundId, cleanupRunLabel: fixture.plan.RunId},
			"Options": map[string]string{"com.docker.network.bridge.name": namespace.bridge, "com.docker.network.bridge.gateway_mode_ipv4": "isolated", "com.docker.network.bridge.gateway_mode_ipv6": "isolated"}})
	})
	return fixture, namespace, networkId
}

// Docker's bridge gains a kernel link-local address when its first peer starts.
// The composed proof must apply the same address policy at both inventory reads.
func TestHostNetworkRetainedReceiptAllowsKernelLinkLocal(t *testing.T) {
	fixture, namespace, networkId := networkLifetimeFixture(t)
	namespace.addresses = []any{map[string]any{"family": "inet6", "local": "fe80::1", "scope": "link", "prefixlen": 64}}
	proof, err := fixture.proxy.proveHostNetwork(t.Context(), networkId)
	if err != nil || validateHostNetworkProof(proof, fixture.plan.RunId, networkId) != nil {
		t.Fatal("kernel link-local bridge failed composed proof", err)
	}
	for _, address := range []map[string]any{
		{"family": "inet", "local": "192.0.2.1", "scope": "global", "prefixlen": 24},
		{"family": "inet6", "local": "2001:db8::1", "scope": "global", "prefixlen": 64},
		{"family": "inet6", "local": "fe80::1", "scope": "global", "prefixlen": 64},
		{"family": "inet6", "local": "fe80::1", "scope": "link", "prefixlen": 32},
	} {
		namespace.addresses = []any{address}
		if _, err := fixture.proxy.proveHostNetwork(t.Context(), networkId); err == nil {
			t.Fatal("composed proof admitted reachable or incorrectly scoped address", address)
		}
	}
}

// This succeeds without opening the idle-admission pin again. A regression to
// fresh admission fails instead of silently renewing a changed generation.
func TestHostNetworkUsesOriginalRuntimeReceipt(t *testing.T) {
	fixture, namespace, networkId := networkLifetimeFixture(t)
	proof, err := fixture.proxy.proveHostNetwork(t.Context(), networkId)
	if err != nil || validateHostNetworkProof(proof, fixture.plan.RunId, networkId) != nil {
		t.Fatal("retained active runtime did not prove its owned bridge", err)
	}
	if namespace.closed || namespace.rechecks == 0 {
		t.Fatal("network query released or skipped the owner's receipt")
	}
	namespace.closed = true
	if _, err := fixture.proxy.proveHostNetwork(t.Context(), networkId); err == nil {
		t.Fatal("retired runtime generation renewed itself")
	}
}

// Closing the owner, not one network request, releases namespace custody.
func TestDockerOwnerCloseReleasesRuntimeReceipt(t *testing.T) {
	fixture, namespace, _ := networkLifetimeFixture(t)
	if err := fixture.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if !namespace.closed {
		t.Fatal("owner leaked its retained native receipt")
	}
}

// Another run's root, device, socket or namespace cannot supply this receipt.
func TestDockerRuntimeReceiptRejectsRebindingAndScopeChanges(t *testing.T) {
	for _, change := range []func(*RuntimeResourceBoundary){
		func(b *RuntimeResourceBoundary) { b.WorkDeviceId++ },
		func(b *RuntimeResourceBoundary) { b.WorkRoot += "/other" },
		func(b *RuntimeResourceBoundary) { b.ScratchRoot += "/other" },
		func(b *RuntimeResourceBoundary) { b.BaseDirectory += "/other" },
		func(b *RuntimeResourceBoundary) { b.PrivateSocketPath += ".other" },
		func(b *RuntimeResourceBoundary) { b.namespace = nil },
	} {
		fixture, _, _ := networkLifetimeFixture(t)
		original := fixture.proxy.runtimeBoundary
		copy := *original
		change(&copy)
		fixture.proxy.runtimeBoundary = nil
		if err := fixture.proxy.bindRuntimeBoundary(&copy); err == nil {
			t.Fatal("runtime receipt accepted a different scope")
		}
		fixture.proxy.runtimeBoundary = original
		if err := fixture.proxy.bindRuntimeBoundary(original); err == nil {
			t.Fatal("runtime generation silently rebound")
		}
	}
}
