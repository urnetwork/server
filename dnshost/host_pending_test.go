//go:build sidecar_pending

package dnshost

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// On §12.14's tables the API role's store reads every root's row, so the block is ready, and its start
// wrote <root>'s own records (DNS-M5-11).
func TestApiHostReadyOnItsTables(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Testing_EnableKeyspaceNotifications(ctx)
		fakeHost := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
		host := fakeHost.ServiceHost()
		if err := fakeHost.Gates.Set("sidecar:\n  dns: {enabled: true}\n"); err != nil {
			t.Fatal(err)
		}
		store, err := NewStore(sidecarhost.NewRedis(nil))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		settings := hostTestSettings(t, nil)
		settings.RootRecords = map[string]*dnsservice.DnsRootRecordSettings{hostTestRoot: {ReportMx: "rpt.report.example"}}
		engine, err := NewApiHost(ctx, host, &HostSettings{Dns: settings, SecretDirectory: t.TempDir()}, &HostDeps{Store: store})
		if err != nil {
			t.Fatal(err)
		}
		api := engine.(*dnsservice.DnsApi)
		defer api.Close()
		for !api.Ready() {
			_, changed := api.Probes()
			if api.Ready() {
				break
			}
			select {
			case <-changed:
			case <-t.Context().Done():
				t.Fatal("never ready")
			}
		}
		state, err := store.RootState(ctx, hostTestRoot)
		if err != nil || state.Serial == 0 {
			t.Fatalf("<root>'s records were not written at start: %+v %v", state, err)
		}
	})
}
