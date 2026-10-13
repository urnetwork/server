// The manager owns one derived-client registration bound and every device it
// opens registers through it.
package proxy

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The default manager bounds registrations; a nonpositive setting is
// unbounded.
func TestProxyDeviceManagerOwnsOneMintAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if settings := DefaultProxyDeviceManagerSettings(); settings.HostedMintConcurrency != defaultHostedMintConcurrency || defaultHostedMintConcurrency <= 0 {
		t.Fatalf("default registration bound = %d", settings.HostedMintConcurrency)
	}
	settings := &ProxyDeviceManagerSettings{HostedMintConcurrency: 2}
	if NewProxyDeviceManager(ctx, settings).mintAdmission == nil {
		t.Fatal("a positive bound built no admission")
	}
	settings.HostedMintConcurrency = 0
	if NewProxyDeviceManager(ctx, settings).mintAdmission != nil {
		t.Fatal("an unbounded manager built an admission")
	}
}

// A device's local authority registers through its manager's admission, so
// one process's devices share one bound.
func TestProxyDeviceAuthoritySharesManagerMintAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		h := setupProxyTestWithOptions(t, defaultProxyTestOptions())
		defer h.close(t)
		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatal(err)
		}
		if h.proxyDeviceManager.mintAdmission == nil {
			t.Fatal("the test manager has no registration bound")
		}
		if pd.localControl == nil || pd.localControl.Testing_MintAdmission() != h.proxyDeviceManager.mintAdmission {
			t.Fatal("the device authority does not register through its manager's bound")
		}
	})
}
