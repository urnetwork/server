//go:build !sidecar_pending

package api

// FS-54 for GET /sidecar/label through the router the API serves: a release leg until MG2's Gn-out links no
// dns_label, so a legacy client's token and the network credential, authenticated against the database, are each
// answered 503, never a label, though a dns root is there to serve.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestSidecarLabelRouteRefusedWithoutMg2(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if server.SidecarMg2Linked() {
			t.Fatal("a release build links MG2")
		}
		self := newRouteAccessDbFixture(tb, ctx)
		defer model.Testing_SetDnsUserRoot("label-route-release.example")()
		_, legacyToken := self.mintClient(t, self.root(), nil)
		for _, authorization := range []string{"Bearer " + legacyToken, self.root(), self.key()} {
			w := self.serve(http.MethodGet, "/sidecar/label", authorization, nil)
			if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), `"label"`) || strings.Contains(w.Body.String(), self.networkId.String()) {
				t.Fatalf("without MG2: %d %s", w.Code, w.Body.String())
			}
		}
	})
}
