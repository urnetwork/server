// The manager owns one contract creation bound and every device it opens
// creates contracts through it.
package proxy

import (
	"context"
	"testing"
)

// The default manager bounds contract creations and hands its bound, with its
// registration bound, to the local authority of every device it opens; a
// nonpositive setting is unbounded.
func TestProxyDeviceManagerOwnsOneContractAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if settings := DefaultProxyDeviceManagerSettings(); settings.HostedContractConcurrency != defaultHostedContractConcurrency || defaultHostedContractConcurrency <= 0 {
		t.Fatalf("default contract creation bound = %d", settings.HostedContractConcurrency)
	}
	settings := &ProxyDeviceManagerSettings{HostedMintConcurrency: 2, HostedContractConcurrency: 2}
	manager := NewProxyDeviceManager(ctx, settings)
	if manager.contractAdmission == nil {
		t.Fatal("a positive bound built no admission")
	}
	authoritySettings := manager.proxyDeviceSettings().authoritySettings()
	if authoritySettings.ContractAdmission != manager.contractAdmission {
		t.Fatal("a device's authority does not create contracts through its manager's bound")
	}
	if authoritySettings.MintAdmission != manager.mintAdmission {
		t.Fatal("a device's authority does not register through its manager's bound")
	}
	settings.HostedContractConcurrency = 0
	if NewProxyDeviceManager(ctx, settings).contractAdmission != nil {
		t.Fatal("an unbounded manager built an admission")
	}
}

// The default admits 256 contract creations in flight per process, far above
// ordinary operation and far below the pile-up after a connect block restart.
// localclient's TestAuthorityContractAdmissionHoldsSlotsUntilControlReturns
// shows a full bound refusing the next creation as a 429.
func TestProxyDeviceManagerDefaultContractBoundIs256(t *testing.T) {
	if settings := DefaultProxyDeviceManagerSettings(); settings.HostedContractConcurrency != 256 {
		t.Fatalf("default contract creation bound = %d, want 256", settings.HostedContractConcurrency)
	}
}
