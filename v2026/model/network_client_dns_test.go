package model

import (
	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestProxyDnsResolverSettingsKeepsExplicitResolver(t *testing.T) {
	custom := &connect.DnsResolverSettings{EnableRemoteDoh: true}
	if got := proxyDnsResolverSettings(custom, "cn"); got != custom {
		t.Fatalf("explicit resolver replaced with regional recommendation: %+v", got)
	}
}

func TestProxyDnsResolverSettingsSelectsRegionalRecommendation(t *testing.T) {
	got := proxyDnsResolverSettings(nil, "cn")
	if got == nil || !got.EnableRemoteDns || got.EnableLocalDns {
		t.Fatalf("regional resolver = %+v, want remote-only recommendation", got)
	}
}

func TestProxyDnsResolverSettingsPreservesDefaultElsewhere(t *testing.T) {
	if got := proxyDnsResolverSettings(nil, "zz"); got != nil {
		t.Fatalf("unsupported country resolver = %+v, want device default", got)
	}
}
