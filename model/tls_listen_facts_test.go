package model

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

// TLS-M5-22's listen facts on a private database: a network that does not exist is an error; a new network's
// standing, verification and tier; its live certificate rows by what they name, against its label, and none of its
// ended or open rows; a mail_network row; a sidecar bound to a hosted device.

// The root the tests' labels are allocated in.
const tlsListenFactsTestRoot = "sidecar.example"

// A P-256 CSR naming names.
func tlsListenFactsTestCsr(t testing.TB, names ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: names}, key)
	if err != nil {
		t.Fatal(err)
	}
	return csr
}

// A self-signed P-256 leaf naming names.
func tlsListenFactsTestLeaf(t testing.TB, notAfter time.Time, names ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
	}
	leaf, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// Writes one certificate row of the network's for sidecarId, in state, through the certs store.
func tlsListenFactsTestCert(t testing.TB, ctx context.Context, networkId server.Id, sidecarId server.Id, state sidecar.TlsCertState, notAfter time.Time, csr []byte, chain [][]byte) {
	t.Helper()
	now := server.NowUtc()
	row := &sidecar.TlsCertRecord{
		CertId:              sidecar.Id(server.NewId()),
		NetworkId:           sidecar.Id(networkId),
		ClientId:            sidecar.Id(server.NewId()),
		SidecarId:           sidecar.Id(sidecarId),
		IdentifierSetSha256: make([]byte, 32),
		SpkiSha256:          make([]byte, 32),
		Attestation:         []byte("attestation"),
		Csr:                 csr,
		State:               state,
		StateTime:           now,
		Chain:               chain,
	}
	if !notAfter.IsZero() {
		row.NotBefore = notAfter.Add(-45 * 24 * time.Hour)
		row.NotAfter = notAfter
		row.Serial = server.NewId().String()
	}
	if err := NewCertsStore(nil).InsertCert(ctx, row); err != nil {
		t.Fatal(err)
	}
}

func TestTlsListenFactsRefuseANetworkThatDoesNotExist(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		facts, err := GetTlsListenFacts(ctx, server.NewId(), server.NowUtc())
		if !errors.Is(err, ErrTlsListenFactsNetworkNotFound) || facts != nil {
			t.Fatalf("a network that does not exist answered %+v, %v", facts, err)
		}
		source := TlsListenFactsSource(nil)
		if _, err := source.ListenFacts(ctx, connect.Id(server.NewId())); !errors.Is(err, ErrTlsListenFactsNetworkNotFound) {
			t.Fatalf("the source answered %v", err)
		}
	})
}

// A new network with a verified owner is verified, on probation and at Community, with no certificate, no mail_network
// row and no hosted sidecar; a legacy guest network is not verified.
func TestTlsListenFactsOfANewNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "tlsfacts"+networkId.String(), server.NewId())
		facts, err := GetTlsListenFacts(ctx, networkId, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		if !facts.Verified || facts.Standing != sidecar.AbuseStandingProbation || facts.Tier != "free" {
			t.Fatalf("a new network's facts are %+v", facts)
		}
		if len(facts.Certs) != 0 || facts.MailNetwork || len(facts.HostedSidecarIds) != 0 || !facts.StandingGrantsEndTime.IsZero() {
			t.Fatalf("a new network holds %+v", facts)
		}
		guestNetworkId := server.NewId()
		Testing_CreateLegacyGuestNetwork(ctx, guestNetworkId, server.NewId())
		guestFacts, err := GetTlsListenFacts(ctx, guestNetworkId, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		if guestFacts.Verified {
			t.Fatal("a guest network reads verified")
		}
		// past probation's age the standing is standard
		later, err := GetTlsListenFacts(ctx, networkId, server.NowUtc().Add(DefaultAbuseSettings().ProbationAge+time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if later.Standing != sidecar.AbuseStandingStandard {
			t.Fatalf("past probation the standing is %s", later.Standing)
		}
	})
}

// Live rows are read by what they name: the network's pair from a CSR or, with none, a leaf; a scope's pair; a set
// for another network's label naming neither. Rows past not_after, revoked, revoking, open or custom are not read.
func TestTlsListenFactsReadTheLiveCertificateRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() || !server.SidecarMg2Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "tlsfacts"+networkId.String(), server.NewId())
		label, err := EnsureNetworkLabel(ctx, networkId, tlsListenFactsTestRoot)
		if err != nil {
			t.Fatal(err)
		}
		zone := label + "." + tlsListenFactsTestRoot
		networkSidecarId, leafSidecarId, scopeSidecarId, strangerSidecarId, endedSidecarId := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		notAfter := now.Add(30 * 24 * time.Hour).Truncate(time.Second)
		tlsListenFactsTestCert(t, ctx, networkId, networkSidecarId, sidecar.TlsCertStateIssued, notAfter, tlsListenFactsTestCsr(t, "*."+zone, zone), nil)
		tlsListenFactsTestCert(t, ctx, networkId, leafSidecarId, sidecar.TlsCertStateIssued, notAfter, nil, [][]byte{tlsListenFactsTestLeaf(t, notAfter, zone, "*."+zone)})
		tlsListenFactsTestCert(t, ctx, networkId, scopeSidecarId, sidecar.TlsCertStateIssued, notAfter, tlsListenFactsTestCsr(t, "*.team."+zone, "team."+zone), nil)
		tlsListenFactsTestCert(t, ctx, networkId, strangerSidecarId, sidecar.TlsCertStateIssued, notAfter, tlsListenFactsTestCsr(t, "*.other."+tlsListenFactsTestRoot, "other."+tlsListenFactsTestRoot), nil)
		// none of these is a live certificate
		tlsListenFactsTestCert(t, ctx, networkId, endedSidecarId, sidecar.TlsCertStateIssued, now.Add(-time.Hour), tlsListenFactsTestCsr(t, "*."+zone, zone), nil)
		tlsListenFactsTestCert(t, ctx, networkId, endedSidecarId, sidecar.TlsCertStateRevoked, notAfter, tlsListenFactsTestCsr(t, "*."+zone, zone), nil)
		tlsListenFactsTestCert(t, ctx, networkId, endedSidecarId, sidecar.TlsCertStateRevoking, notAfter, tlsListenFactsTestCsr(t, "*."+zone, zone), nil)
		tlsListenFactsTestCert(t, ctx, networkId, endedSidecarId, sidecar.TlsCertStateQueued, time.Time{}, tlsListenFactsTestCsr(t, "*."+zone, zone), nil)
		tlsListenFactsTestCert(t, ctx, networkId, endedSidecarId, sidecar.TlsCertStateCustomIssued, notAfter, tlsListenFactsTestCsr(t, "app.customer.example"), nil)
		// another network's row is not this network's
		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, otherNetworkId, "tlsfacts"+otherNetworkId.String(), server.NewId())
		tlsListenFactsTestCert(t, ctx, otherNetworkId, endedSidecarId, sidecar.TlsCertStateIssued, notAfter, tlsListenFactsTestCsr(t, "*."+zone, zone), nil)

		facts, err := GetTlsListenFacts(ctx, networkId, now)
		if err != nil {
			t.Fatal(err)
		}
		sidecarIdCerts := map[connect.Id]*socketsservice.TlsListenCert{}
		for _, cert := range facts.Certs {
			if _, found := sidecarIdCerts[cert.SidecarId]; found {
				t.Fatalf("two live rows for %s", cert.SidecarId)
			}
			sidecarIdCerts[cert.SidecarId] = cert
		}
		if len(sidecarIdCerts) != 4 {
			t.Fatalf("%d live rows: %+v", len(sidecarIdCerts), facts.Certs)
		}
		for _, sidecarId := range []server.Id{networkSidecarId, leafSidecarId} {
			cert := sidecarIdCerts[connect.Id(sidecarId)]
			if cert == nil || !cert.NetworkPair || len(cert.ScopePairs) != 0 || !cert.NotAfter.Equal(notAfter) {
				t.Fatalf("a network pair's row reads %+v", cert)
			}
			if !facts.Certified(connect.Id(sidecarId), "www", now) || !facts.Certified(connect.Id(sidecarId), "@", now) {
				t.Fatal("a network pair certifies no network host")
			}
			if facts.Certified(connect.Id(sidecarId), "www", notAfter) {
				t.Fatal("a row certifies at its not_after")
			}
		}
		if cert := sidecarIdCerts[connect.Id(scopeSidecarId)]; cert == nil || cert.NetworkPair || !slices.Equal(cert.ScopePairs, []string{"team"}) {
			t.Fatalf("a scope's row reads %+v", cert)
		}
		if facts.Certified(connect.Id(scopeSidecarId), "www", now) || !facts.Certified(connect.Id(scopeSidecarId), "www.team", now) {
			t.Fatal("a scope's row certifies the wrong hosts")
		}
		if cert := sidecarIdCerts[connect.Id(strangerSidecarId)]; cert == nil || cert.NetworkPair || len(cert.ScopePairs) != 0 {
			t.Fatalf("another label's row reads %+v", cert)
		}
		if facts.Certified(connect.Id(endedSidecarId), "www", now) {
			t.Fatal("an ended row certifies")
		}
		// past the cap the read refuses rather than judge part of the rows
		capped := DefaultTlsListenFactsSettings()
		capped.CertMaxCount = 3
		if _, err := GetTlsListenFactsWithSettings(ctx, networkId, now, capped); !errors.Is(err, ErrTlsListenFactsTooManyCerts) {
			t.Fatalf("past the cap answered %v", err)
		}
	})
}

// A mail_network row is read, and a sidecar bound to a hosted device, or to its child, is marked; one bound to an
// ordinary client is not.
func TestTlsListenFactsReadMailAndHostedSidecars(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() || !server.SidecarMg1Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "tlsfacts"+networkId.String(), server.NewId())
		hostedClientId, childClientId, ordinaryClientId := server.NewId(), server.NewId(), server.NewId()
		hostedSidecarId, childSidecarId, ordinarySidecarId := server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO mail_network (network_id, create_time) VALUES ($1, $2)`, networkId, now))
			for _, clientId := range []server.Id{hostedClientId, ordinaryClientId} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`, clientId, networkId))
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_client (client_id, network_id, active, source_client_id) VALUES ($1, $2, true, $3)`,
				childClientId,
				networkId,
				hostedClientId,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO proxy_device_config (proxy_id, client_id, instance_id, config_json) VALUES ($1, $2, $3, '{}')`,
				server.NewId(),
				hostedClientId,
				server.NewId(),
			))
			for sidecarId, clientId := range map[server.Id]server.Id{hostedSidecarId: hostedClientId, childSidecarId: childClientId, ordinarySidecarId: ordinaryClientId} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO sidecar_identity (sidecar_id, network_id, identity_public_key, identity_alg, status, create_time)
						VALUES ($1, $2, $3, 'ed25519', 'active', $4)
					`,
					sidecarId,
					networkId,
					make([]byte, 32),
					now,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
						VALUES ($1, 1, $2, $3, $4, $5)
					`,
					sidecarId,
					clientId,
					make([]byte, 32),
					[]byte("signatures"),
					now,
				))
			}
		})
		facts, err := GetTlsListenFacts(ctx, networkId, now)
		if err != nil {
			t.Fatal(err)
		}
		if !facts.MailNetwork {
			t.Fatal("the mail_network row is not read")
		}
		hosted := slices.Clone(facts.HostedSidecarIds)
		want := []connect.Id{connect.Id(hostedSidecarId), connect.Id(childSidecarId)}
		slices.SortFunc(hosted, func(a connect.Id, b connect.Id) int { return a.Cmp(b) })
		slices.SortFunc(want, func(a connect.Id, b connect.Id) int { return a.Cmp(b) })
		if !slices.Equal(hosted, want) {
			t.Fatalf("hosted sidecars %v, want %v", hosted, want)
		}
	})
}
