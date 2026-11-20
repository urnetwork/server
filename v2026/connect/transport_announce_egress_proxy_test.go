package connect

// An announcement of a transport that declared a proxy egress marks its client
// before its lease attributes the address (transport_egress_proxy.go), so the
// verify index never attributes a declared transport to an unmarked client.
// One that declared nothing leaves its client unmarked.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

func TestConnectionAnnounceMarksADeclaredProxyEgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// one address each, so the first client's lease never shares the second's
		for proxiedEgress, clientAddress := range map[bool]string{true: "192.0.2.11:20000", false: "192.0.2.12:20000"} {
			testCtx, testCancel := context.WithTimeout(ctx, 30*time.Second)
			fixture := newLocationAnnounceFixture(ctx)
			verifySettings := model.DefaultVerifySettings()
			verifySettings.EgressHashKey = []byte("announce-egress-proxy-test")
			fixture.settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
				return verifySettings, true
			}
			fixture.announce = newConnectionAnnounceWithReconcile(
				controller.Testing_WithConnectionLocationWriter(fixture.transportCtx, func(context.Context, server.Id, string) error {
					return nil
				}),
				fixture.transportCancel,
				fixture.networkId,
				fixture.clientId,
				clientAddress,
				0,
				proxiedEgress,
				"",
				fixture.handlerId,
				nil,
				0,
				V0TestConfig(),
				fixture.settings,
			)
			ip, ok := model.ParseVerifyEgressIp(clientAddress)
			if !ok {
				t.Fatal("the fixture address does not parse")
			}
			// once the lease attributes the address, the mark is what the
			// transport declared
			for {
				if owner := model.ResolveVerifyEgress(ctx, ip, verifySettings); owner != nil && *owner == fixture.clientId {
					break
				}
				select {
				case <-testCtx.Done():
					t.Fatalf("proxied %v: the lease never attributed the address", proxiedEgress)
				case <-time.After(20 * time.Millisecond):
				}
			}
			if marked := model.GetVerifyProxiedEgress(ctx, []server.Id{fixture.clientId})[fixture.clientId]; marked != proxiedEgress {
				t.Fatalf("a transport that declared a proxy egress %v left its client marked %v", proxiedEgress, marked)
			}
			fixture.announce.CloseAndWait()
			testCancel()
		}
	})
}
