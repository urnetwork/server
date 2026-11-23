//go:build sidecar_pending

package model

// §12.7's Team scope zones (dns_scope_model.go; DNS-M6-06, DNS-M8-01) on Postgres: a scope's set written
// through the journal from the network's function, once a change; a scope label refused in each listed case,
// and an unknown scope or a process with no source refused; the record API judging each owner in its zone, a
// user owner equal to a scope label and a host two labels deep refused; a scope withdrawn whole; a scope's @
// refused as a custom host's owner; the report token minted once and kept; and a scope's mail host taken by
// the key-record write only for a scope the projection lists.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The zone's rows at or below owner, as lines.
func testDnsZoneLinesBelow(t testing.TB, ctx context.Context, root string, networkId server.Id, owner string) []string {
	t.Helper()
	sets := []dnsservice.DnsRecordSet{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, row := range getDnsZoneRowsInTx(ctx, conn, root, networkId) {
			if dnsOwnerAtOrBelow(row.set.Owner, owner) {
				sets = append(sets, row.set)
			}
		}
	})
	return dnsSystemRecordLines(sets)
}

func testBool(value bool) *bool {
	return &value
}

// A scope's set goes through the journal as the network's function at the scope's apex: one write for the
// new scope, none for the same state again, one for the edge rows and one for its mail, which brings its own
// mx host.
func TestDnsSetScopeRecordsWritesTheScopesSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		testDnsScopeSource(t, networkId, "eng")
		scopeApex := "eng." + apex
		before := testDnsSerialNow(t, ctx, root)
		result, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "eng"})
		if err != nil || result == nil || result.Serial != before+1 {
			t.Fatalf("first write %+v %v", result, err)
		}
		state := &DnsScopeState{Root: root, Apex: apex, Scope: "eng"}
		server.Tx(ctx, func(tx server.PgTx) {
			state.MailPolicy = getDnsZoneMailPolicyInTx(ctx, tx, root, networkId)
		})
		want, err := DnsScopeRecords(state, dnsZoneTtl())
		if err != nil {
			t.Fatal(err)
		}
		// the scope's apex takes no copy, and _dmarc keeps *.<scope>'s records through one (QD-1190)
		wantLines := testDnsWithCopyLines(dnsSystemRecordLines(want), "_dmarc."+scopeApex)
		if got := testDnsZoneLinesBelow(t, ctx, root, networkId, scopeApex); !slices.Equal(got, wantLines) {
			t.Fatalf("the scope's rows:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantLines, "\n"))
		}
		if again, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "eng"}); err != nil || again != nil {
			t.Fatalf("the same state again wrote %+v %v", again, err)
		}
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "eng", EdgeWildcard: testBool(true)}); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "eng", Mail: testBool(true)}); err != nil {
			t.Fatal(err)
		}
		lines := testDnsZoneLinesBelow(t, ctx, root, networkId, scopeApex)
		for _, line := range []string{"*." + scopeApex + " A edge ", "mx." + scopeApex + " A router ", scopeApex + " MX system 10 mx." + scopeApex} {
			if !slices.Contains(lines, line) {
				t.Errorf("no %q in\n%s", line, strings.Join(lines, "\n"))
			}
		}
		// the edge rows stood through the mail write: the state is read back from the scope's rows
		state.EdgeWildcard = true
		state.Mail = true
		// _dmarc and b keep *.<scope>'s records through copies, and the scope holds no _mta-sts to take one, its
		// network verified though it is (QD-1206); mx's router addresses are its own
		copyOwners := []string{"_dmarc." + scopeApex, "b." + scopeApex}
		if want, _ := DnsScopeRecords(state, dnsZoneTtl()); !slices.Equal(lines, testDnsWithCopyLines(dnsSystemRecordLines(want), copyOwners...)) {
			t.Errorf("after edge and mail:\n%s", strings.Join(lines, "\n"))
		}
		// none of it is an entry
		if count := DnsEntryCount(ctx, networkId); count != 0 {
			t.Errorf("%d entries", count)
		}
	})
}

func testDnsSerialNow(t testing.TB, ctx context.Context, root string) uint64 {
	t.Helper()
	state, err := GetDnsRootState(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	return state.Serial
}

// A new scope's label is refused in each listed case, with nothing written: reserved, one of §12.7's system
// hosts, a word-number label, a host the zone holds and a client name; and a scope the projection does not list,
// or any scope with no source, is unknown.
func TestDnsSetScopeRecordsRefusesEachLabel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := world.write(ctx, testDnsUpsert("shop."+world.apex, "TXT", `"hello"`)); err != nil {
			t.Fatal(err)
		}
		testDnsScopeSource(t, world.networkId, "www", "b", "maple-1234", "shop", "laptop")
		before := testDnsSerialNow(t, ctx, world.root)
		for _, scope := range []string{"www", "b", "maple-1234", "shop", "laptop"} {
			_, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: world.root, NetworkId: world.networkId, Scope: scope, ClientNames: []string{"laptop"}})
			if !errors.Is(err, dnsservice.ErrDnsScopeLabelRefused) {
				t.Errorf("%s: %v", scope, err)
			}
		}
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: world.root, NetworkId: world.networkId, Scope: "eng"}); !errors.Is(err, ErrDnsScopeUnknown) {
			t.Errorf("a scope the projection does not list: %v", err)
		}
		SetDnsScopeLabelSource(nil)
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: world.root, NetworkId: world.networkId, Scope: "www"}); !errors.Is(err, ErrDnsScopeUnknown) {
			t.Errorf("no source: %v", err)
		}
		if after := testDnsSerialNow(t, ctx, world.root); after != before {
			t.Errorf("a refusal took serial %d", after)
		}
	})
}

// The record API judges each owner in its zone: a scope's host takes a record once the scope's set is written;
// the scope's apex, an underscore owner at it, a host two labels deep and a name under no scope are refused; a
// user owner equal to a scope label is refused in the network's zone; a scope whose set is not written takes
// nothing; and a process with no source refuses every scope host.
func TestDnsRecordsJudgeEachOwnerInItsScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		testDnsScopeSource(t, world.networkId, "eng", "ops")
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: world.root, NetworkId: world.networkId, Scope: "eng"}); err != nil {
			t.Fatal(err)
		}
		scopeApex := "eng." + world.apex
		if _, err := world.write(ctx, testDnsUpsert("web."+scopeApex, "A", "11.22.33.44")); err != nil {
			t.Fatalf("a scope host: %v", err)
		}
		if _, err := world.write(ctx, testDnsUpsert("*.web."+scopeApex, "A", "11.22.33.44")); err != nil {
			t.Fatalf("a scope host's wildcard: %v", err)
		}
		refused := []string{scopeApex, "a.web." + scopeApex, "x.other." + world.apex, "web.ops." + world.apex}
		for _, owner := range refused {
			if _, err := world.write(ctx, testDnsUpsert(owner, "A", "11.22.33.44")); testDnsApiError(t, err).Status != http.StatusBadRequest {
				t.Errorf("%s: %v", owner, err)
			}
		}
		if _, err := world.write(ctx, testDnsUpsert("_verify."+scopeApex, "TXT", `"x"`)); testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("an underscore owner at the scope's apex: %v", err)
		}
		// in the network's zone a scope label is no user owner, set or not
		if _, err := world.write(ctx, testDnsUpsert("ops."+world.apex, "A", "11.22.33.44")); testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("a scope label as a host: %v", err)
		}
		// the scope's system rows are the system's
		if _, err := world.write(ctx, &sidecar.DnsRecordsRequest{Deletes: []sidecar.DnsRecordDelete{{Name: scopeApex, Type: "MX"}}}); testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("a delete of the scope's MX: %v", err)
		}
		// the user record under the scope counts as the network's entry
		if count := DnsEntryCount(ctx, world.networkId); count != 2 {
			t.Errorf("%d entries, want 2", count)
		}
		SetDnsScopeLabelSource(nil)
		if _, err := world.write(ctx, testDnsUpsert("mail2.web."+scopeApex, "A", "11.22.33.44")); testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("no source: %v", err)
		}
	})
}

// A scope leaving the projection is withdrawn whole: its set and its users' rows in one write.
func TestDnsRemoveScopeRecordsWithdrawsTheScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		testDnsScopeSource(t, world.networkId, "eng")
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: world.root, NetworkId: world.networkId, Scope: "eng"}); err != nil {
			t.Fatal(err)
		}
		scopeApex := "eng." + world.apex
		if _, err := world.write(ctx, testDnsUpsert("web."+scopeApex, "A", "11.22.33.44")); err != nil {
			t.Fatal(err)
		}
		before := testDnsSerialNow(t, ctx, world.root)
		var result *DnsWriteResult
		var returnErr error
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsRemoveScopeRecordsInTx(ctx, tx, world.networkId, world.root, "eng")
		}, server.TxReadCommitted)
		if returnErr != nil || result == nil || result.Serial != before+1 {
			t.Fatalf("%+v %v", result, returnErr)
		}
		if lines := testDnsZoneLinesBelow(t, ctx, world.root, world.networkId, scopeApex); len(lines) != 0 {
			t.Errorf("left %v", lines)
		}
		// the network's own set stands
		if lines := testDnsZoneLinesBelow(t, ctx, world.root, world.networkId, world.apex); !slices.Contains(lines, world.apex+" TXT system \"v=spf1 -all\"") {
			t.Errorf("the network's set went: %v", lines)
		}
	})
}

// A scope's @ maps no custom host, by the projection's label and by the scope's written set alike.
func TestDnsCustomHostRefusesAScopeApex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		testDnsScopeSource(t, world.networkId, "eng")
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: world.root, NetworkId: world.networkId, Scope: "eng"}); err != nil {
			t.Fatal(err)
		}
		_, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: "eng." + world.apex})
		if testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("a scope's apex: %v", err)
		}
		// with the source gone, the written set still marks the scope's apex
		SetDnsScopeLabelSource(nil)
		_, err = world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "www." + world.domain, Owner: "eng." + world.apex})
		if testDnsApiError(t, err).Status != http.StatusBadRequest {
			t.Errorf("a written scope's apex with no source: %v", err)
		}
	})
}

// The report token is minted with the zone's set, once: a second write keeps it, another network's differs, and
// _dmarc's rua names it at the configured report host (DNS-M8-01, plan-DNS-rua-token).
func TestDnsReportTokenIsMintedOnceAndKept(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		first, _ := testDnsNetwork(t, ctx, root)
		second, _ := testDnsNetwork(t, ctx, root)
		token := func(networkId server.Id) string {
			t.Helper()
			if _, err := EnsureNetworkZone(ctx, networkId, root); err != nil {
				t.Fatal(err)
			}
			var token *string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT report_token FROM network_dns_zone WHERE root = $1 AND network_id = $2`, root, networkId).Scan(&token))
			})
			if token == nil || len(*token) != 26 || strings.ToLower(*token) != *token {
				t.Fatalf("token %v", token)
			}
			return *token
		}
		firstToken := token(first)
		if again := token(first); again != firstToken {
			t.Errorf("the token moved: %s %s", firstToken, again)
		}
		if other := token(second); other == firstToken {
			t.Error("two networks share a token")
		}
		if networkId, ok := GetDnsNetworkForReportToken(ctx, firstToken); !ok || networkId != first {
			t.Errorf("the token maps to %v %t", networkId, ok)
		}
		if _, ok := GetDnsNetworkForReportToken(ctx, strings.Repeat("a", 26)); ok {
			t.Error("an unknown token maps")
		}
	})
}

// POST /dns/key-record's write takes a scope's mail host only for a scope the projection lists: an unknown
// scope, and any scope with no source, is refused before the attestation is read.
func TestDnsKeyRecordRefusesAnUnknownScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, _ := testDnsNetwork(t, ctx, root)
		publish := func() error {
			_, err := PublishDnsKeyRecord(ctx, &DnsKeyRecord{
				Root:              root,
				NetworkId:         networkId,
				ReceiverSidecarId: server.NewId(),
				Request:           &sidecar.DnsKeyRecordRequest{Host: "mx.eng", Spki: testDnsScopeSpki(t), Attestation: []byte("x")},
			})
			return err
		}
		if err := publish(); testDnsApiError(t, err).Status != http.StatusForbidden || !strings.Contains(err.Error(), "scope") {
			t.Errorf("no source: %v", err)
		}
		testDnsScopeSource(t, networkId, "ops")
		if err := publish(); testDnsApiError(t, err).Status != http.StatusForbidden || !strings.Contains(err.Error(), "scope") {
			t.Errorf("an unknown scope: %v", err)
		}
	})
}

// A P-256 SubjectPublicKeyInfo of a fresh key.
func testDnsScopeSpki(t testing.TB) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return spki
}
