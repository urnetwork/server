// Observe both PostgreSQL routes with the qualified frame observer. No delay,
// reply barrier, SQL rewrite or task scheduling change is enabled here.
package model

import (
	"context"
	"maps"
	"sync"
	"testing"

	"github.com/urnetwork/server"
	"gopkg.in/yaml.v3"
)

type legacyFinancialRunProtocol struct {
	proxies map[string]*legacyCohortLatencyProxy
}

// Both resources keep their original credentials, database and pool settings.
// An absent maintenance resource originally inherits the ordinary resource.
func legacyFinancialRunProtocolBind(t testing.TB, ctx context.Context) (*legacyFinancialRunProtocol, func()) {
	t.Helper()
	ordinary := server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName)
	maintenance, err := server.Vault.SimpleResource(server.MaintenancePgVaultResourceName)
	if err != nil {
		maintenance = ordinary
	}
	if ordinary.RequireString("db") != maintenance.RequireString("db") {
		t.Fatal("protocol observer requires both routes in the same disposable database")
	}
	observer := &legacyFinancialRunProtocol{proxies: map[string]*legacyCohortLatencyProxy{}}
	var pops []func()
	var closeOnce sync.Once
	close := func() {
		closeOnce.Do(func() {
			for _, proxy := range observer.proxies {
				proxy.close()
			}
			server.PgReset()
			for index := len(pops) - 1; index >= 0; index-- {
				pops[index]()
			}
		})
	}
	ready := false
	defer func() {
		if !ready {
			close()
		}
	}()
	for _, route := range []struct {
		name     string
		resource *server.SimpleResource
	}{
		{name: server.DefaultPgVaultResourceName, resource: ordinary},
		{name: server.MaintenancePgVaultResourceName, resource: maintenance},
	} {
		proxy := newLegacyCohortLatencyProxy(t, ctx, route.resource.RequireString("authority"))
		observer.proxies[route.name] = proxy
		values := maps.Clone(route.resource.Parse())
		values["authority"] = proxy.listener.Addr().String()
		encoded, err := yaml.Marshal(values)
		server.Raise(err)
		pops = append(pops, server.Vault.PushSimpleResource(route.name, encoded))
	}
	server.PgReset()
	ready = true
	return observer, close
}

func (self *legacyFinancialRunProtocol) enabled(enabled bool) {
	for _, proxy := range self.proxies {
		proxy.enabled.Store(enabled)
	}
}

func (self *legacyFinancialRunProtocol) snapshot() map[string]map[string]int64 {
	result := map[string]map[string]int64{}
	for name, proxy := range self.proxies {
		result[name] = proxy.snapshot()
	}
	return result
}

// Cumulative counter subtraction preserves the actual owner phase. A maximum
// over both phases cannot be subtracted to recover an owner-only maximum.
func legacyFinancialRunProtocolDelta(before, after map[string]map[string]int64) map[string]map[string]int64 {
	result := map[string]map[string]int64{}
	for name, counters := range after {
		result[name] = map[string]int64{}
		for key, value := range counters {
			if key != "max_begin_to_end_command_observed_ns" {
				result[name][key] = value - before[name][key]
			}
		}
	}
	return result
}
