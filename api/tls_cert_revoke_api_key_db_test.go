//go:build sidecar_pending

package api

// POST /tls/cert-revoke under an API key, end to end through the router the
// API serves, against the database with MG0 (QD-847): the handler reads the
// key's scopes from its row and passes them to the controller, so a key whose
// scopes reach devices revokes another client's certificate of its network and
// one whose scopes stop short of devices is refused. The readings the route
// already had hold through the same router: the owner's token any certificate
// of its network, a client token its own client's alone.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// The certificate routes' deps over a fake store, installed for the test.
func installTlsCertsForTest(t testing.TB, ctx context.Context) (*sidecartest.FakeHost, *sidecartest.FakeCertsStore) {
	t.Helper()
	host := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil)
	sidecartest.RegisterCertsBudgetTwins(host.Redis)
	store := sidecartest.NewFakeCertsStore(nil)
	settings := certsservice.DefaultCertsSettings()
	settings.RootDomain = "root.example"
	service, err := certsservice.NewCertsService(ctx, host.ServiceHost(), store, nil, nil, settings)
	if err != nil {
		t.Fatal(err)
	}
	controller.SetTlsCerts(&controller.TlsCertsDeps{
		Service:       service,
		Store:         store,
		Authenticator: host.Authenticator,
		Subject: func(ctx context.Context, principal *sidecar.Principal, now time.Time) (*controller.TlsCertSubject, error) {
			return &controller.TlsCertSubject{}, nil
		},
	})
	return host, store
}

func TestTlsCertRevokeReadsTheApiKeysScopes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f := newRouteAccessDbFixture(t, ctx)
		host, store := installTlsCertsForTest(t, ctx)
		defer controller.SetTlsCerts(nil)

		// an issued certificate of clientId in networkId
		issued := func(networkId server.Id, clientId server.Id) sidecar.Id {
			t.Helper()
			now := host.Clock.Now()
			row := &sidecar.TlsCertRecord{
				CertId:              sidecartest.CertsIdAt(now),
				NetworkId:           sidecar.Id(networkId),
				ClientId:            sidecar.Id(clientId),
				SidecarId:           sidecar.NewId(),
				IdentifierSetSha256: make([]byte, 32),
				SpkiSha256:          make([]byte, 32),
				Attestation:         []byte("attestation"),
				Csr:                 []byte("csr"),
				Ca:                  string(certsservice.CertsCaLetsEncrypt),
				Serial:              "0a",
				NotBefore:           now,
				NotAfter:            now.Add(45 * 24 * time.Hour),
				State:               sidecar.TlsCertStateIssued,
				StateTime:           now,
				Chain:               [][]byte{[]byte("leaf"), []byte("intermediate")},
			}
			if err := store.InsertCert(ctx, row); err != nil {
				t.Fatal(err)
			}
			return row.CertId
		}
		state := func(certId sidecar.Id) sidecar.TlsCertState {
			t.Helper()
			row, err := store.GetCert(ctx, certId)
			if err != nil {
				t.Fatal(err)
			}
			return row.State
		}
		// the key's scopes column, as its row would hold them: nil for a legacy key's NULL
		setScopes := func(scopesJson *string) {
			t.Helper()
			server.Tx(ctx, func(tx server.PgTx) {
				tag, err := tx.Exec(ctx, `UPDATE account_api_key SET scopes = $2::jsonb WHERE api_key_id = $1`, f.apiKeyId, scopesJson)
				server.Raise(err)
				if tag.RowsAffected() != 1 {
					t.Fatalf("the key's scopes changed %d rows", tag.RowsAffected())
				}
			})
		}
		revoke := func(authorization string, certId sidecar.Id, status int) {
			t.Helper()
			var result controller.TlsCertRevokeResult
			var out any
			if status == http.StatusOK {
				out = &result
			}
			f.call(t, http.MethodPost, "/tls/cert-revoke", authorization, &sidecar.TlsCertRevokeArgs{CertId: certId}, status, out)
			if status == http.StatusOK {
				if result.State != sidecar.TlsCertStateRevoking || state(certId) != sidecar.TlsCertStateRevoking {
					t.Fatalf("a revocation answered %q and left the row %q", result.State, state(certId))
				}
			} else if state(certId) != sidecar.TlsCertStateIssued {
				t.Fatalf("a revocation refused %d left the row %q", status, state(certId))
			}
		}

		otherClientId := server.NewId()
		strangers := issued(server.NewId(), server.NewId())

		// the key as the route made it, with scope all: another client's certificate of its network
		revoke(f.key(), issued(f.networkId, otherClientId), http.StatusOK)

		// scopes that stop short of devices, and a column that holds none or does not parse
		refused := issued(f.networkId, otherClientId)
		for _, scopesJson := range []string{`["policy:read"]`, `["logs:read", "auth_keys"]`, `[]`, `{"all": true}`, `["devices"]`} {
			setScopes(&scopesJson)
			revoke(f.key(), refused, http.StatusForbidden)
		}

		// scopes that reach devices, and a legacy key's none of its own
		devicesJson := `["devices:core"]`
		setScopes(&devicesJson)
		revoke(f.key(), issued(f.networkId, otherClientId), http.StatusOK)
		// still none of another network's
		revoke(f.key(), strangers, http.StatusNotFound)
		setScopes(nil)
		revoke(f.key(), issued(f.networkId, otherClientId), http.StatusOK)

		// a key that is no key any more reaches nothing
		noneJson := `["policy:read"]`
		setScopes(&noneJson)
		revoke("Bearer "+f.apiKey[:len(f.apiKey)-1]+"0", refused, http.StatusUnauthorized)

		// the readings the route had: the owner's token any of its network's, a client token its own client's alone
		revoke(f.root(), refused, http.StatusOK)
		revoke(f.root(), strangers, http.StatusNotFound)
		clientId, clientToken := f.mintClient(t, f.root(), nil)
		revoke("Bearer "+clientToken, issued(f.networkId, otherClientId), http.StatusForbidden)
		revoke("Bearer "+clientToken, issued(f.networkId, clientId), http.StatusOK)
	})
}
