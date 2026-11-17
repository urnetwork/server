//go:build !sidecar_pending

package controller

// FS-54 for GET /sidecar/label: a release leg until MG2's Gn-out links no dns_label, so the route answers 503
// unavailable without a query, never a label, though a dns root is there to serve.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestSidecarLabelRefusedWithoutMg2(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if server.SidecarMg2Linked() {
			t.Fatal("a release build links MG2")
		}
		defer model.Testing_SetDnsUserRoot("label-release.example")()
		network := session.NewByJwt(server.NewId(), server.NewId(), "labelrelease", false, false)
		for _, byJwt := range []*session.ByJwt{network.Client(server.NewId(), server.NewId()), network} {
			clientSession := session.Testing_CreateClientSession(ctx, byJwt)
			attempts, stop := server.DenyPostgresForTest(t)
			answer, err := GetSidecarLabel(clientSession)
			stop()
			connect.AssertEqual(t, attempts(), uint64(0))
			var apiErr *sidecar.DnsApiError
			if answer != nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != sidecar.DnsApiCodeUnavailable {
				t.Fatalf("without MG2: %+v, %v", answer, err)
			}
		}
	})
}
