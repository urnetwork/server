//go:build sidecar_pending

package model

// TLS-M6-03's alias half of the issue (DNS's reading 11; §13.8's set row, I-14): a chain issued for a set naming a
// live alias's pair certifies that alias in the issue's own transaction, so the alias answers its label's records
// from that write on; a set naming the label's pair alone certifies nothing, and a retiring alias takes no
// certificate while the issue still lands.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

// A row of the world's network ordering set's CSR for a sidecar of its own, and a chain for it.
func testTlsCertAliasRow(t testing.TB, ctx context.Context, world *testDnsRecordsWorld, set []string) (sidecar.Id, [][]byte) {
	certId, chain, _ := testTlsCertAliasRowOf(t, ctx, world, set)
	return certId, chain
}

// testTlsCertAliasRow, with the row's sidecar.
func testTlsCertAliasRowOf(t testing.TB, ctx context.Context, world *testDnsRecordsWorld, set []string) (sidecar.Id, [][]byte, server.Id) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: set}, key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: set[0]}, DNSNames: set,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(45 * 24 * time.Hour),
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test issuer"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	identifierSetSha256, err := sidecar.TlsIdentifierSetHash(set)
	if err != nil {
		t.Fatal(err)
	}
	certId := server.NewId()
	sidecarId := server.NewId()
	spki := sha256.Sum256(certId.Bytes())
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_tls_cert (
					cert_id, network_id, client_id, sidecar_id, identifier_set_sha256, spki_sha256, attestation, csr, state, state_time
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'ordering', $9)
			`,
			certId, world.networkId, server.NewId(), sidecarId, identifierSetSha256[:], spki[:], []byte{0x01}, csr, server.NowUtc(),
		))
	})
	return sidecar.Id(certId), [][]byte{leaf}, sidecarId
}

// The row's issue as certs' order chain moves it.
func testTlsCertIssue(t testing.TB, ctx context.Context, certId sidecar.Id, chain [][]byte) bool {
	t.Helper()
	now := server.NowUtc()
	applied, err := NewCertsStore(nil).TransitionCert(ctx, &certsservice.CertsTransition{
		CertId:    certId,
		From:      []sidecar.TlsCertState{sidecar.TlsCertStateOrdering},
		To:        sidecar.TlsCertStateIssued,
		At:        now,
		Serial:    "01",
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(45 * 24 * time.Hour),
		Chain:     chain,
	})
	if err != nil {
		t.Fatal(err)
	}
	return applied
}

// Whether the network's alias is certified, as GET /network/dns shows it.
func testTlsAliasCertified(t testing.TB, ctx context.Context, world *testDnsRecordsWorld) bool {
	t.Helper()
	zone, err := GetNetworkDns(ctx, world.root, world.networkId)
	if err != nil {
		t.Fatal(err)
	}
	return zone.AliasCertified
}

func TestTlsCertIssueCertifiesTheAliasesItsSetNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := world.claimAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		root := strings.TrimSuffix(world.root, ".")

		// the label's pair alone certifies nothing
		labelSet, err := sidecar.TlsIdentifierSet(root, world.label(), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		labelCertId, labelChain := testTlsCertAliasRow(t, ctx, world, labelSet)
		if !testTlsCertIssue(t, ctx, labelCertId, labelChain) {
			t.Fatal("the label's issue did not apply")
		}
		if testTlsAliasCertified(t, ctx, world) {
			t.Fatal("a certificate for the label alone certified the alias")
		}

		// the network's set with the alias's pair certifies it at the issue
		aliasSet, err := sidecar.TlsIdentifierSet(root, world.label(), "", []string{name})
		if err != nil {
			t.Fatal(err)
		}
		aliasCertId, aliasChain, aliasSidecarId := testTlsCertAliasRowOf(t, ctx, world, aliasSet)
		if !testTlsCertIssue(t, ctx, aliasCertId, aliasChain) {
			t.Fatal("the alias's issue did not apply")
		}
		if !testTlsAliasCertified(t, ctx, world) {
			t.Fatal("the issue of a certificate naming the alias left it uncertified")
		}
		// ValidateListen's facts carry the certified alias for the row's sidecar, on every owner of the zone
		facts, err := GetTlsListenFacts(ctx, world.networkId, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, custom := range facts.CustomCertified {
			if custom.Owner == socketsservice.TlsListenCustomNameAnyOwner && custom.Name == name+"."+root {
				if custom.SidecarId != connect.Id(aliasSidecarId) || !server.NowUtc().Before(custom.NotAfter) {
					t.Fatalf("the certified alias's facts %+v", custom)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("the facts hold no certified alias: %+v", facts.CustomCertified)
		}
		// a second issue for it certifies nothing more and still lands
		againCertId, againChain := testTlsCertAliasRow(t, ctx, world, aliasSet)
		if !testTlsCertIssue(t, ctx, againCertId, againChain) {
			t.Fatal("a second issue for the alias did not apply")
		}
	})
}

func TestTlsCertIssueOfARetiringAliasStillLands(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := world.claimAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		if _, err := world.releaseAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		aliasSet, err := sidecar.TlsIdentifierSet(strings.TrimSuffix(world.root, "."), world.label(), "", []string{name})
		if err != nil {
			t.Fatal(err)
		}
		certId, chain := testTlsCertAliasRow(t, ctx, world, aliasSet)
		if !testTlsCertIssue(t, ctx, certId, chain) {
			t.Fatal("the issue naming a retiring alias did not apply")
		}
		if testTlsAliasCertified(t, ctx, world) {
			t.Fatal("a retiring alias took a certificate")
		}
	})
}

// The pairs a set names: an alias's whole pair in a root the network holds, never the label's, a name with no
// wildcard beside it, or one of another root.
func TestTlsCertAliasPairs(t *testing.T) {
	roots := []string{"root.example."}
	labels := map[string]string{"root.example.": "abcdefghij"}
	pairs := tlsCertAliasPairs([]string{
		"*.abcdefghij.root.example", "abcdefghij.root.example",
		"*.shop.root.example", "shop.root.example",
		"blog.root.example",
		"*.other.elsewhere.example", "other.elsewhere.example",
		"*.team.abcdefghij.root.example", "team.abcdefghij.root.example",
	}, roots, labels)
	if aliases := pairs["root.example."]; len(aliases) != 1 || aliases[0] != "shop" {
		t.Fatalf("pairs %v", pairs)
	}
}
