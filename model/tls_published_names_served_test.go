package model

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

// The join of a network's grants to the names it serves them under (QD-770):
// each live label and alias in every root, each active custom host under a
// verified domain whose owner is the grant's host, and a grant with none of
// these once with no name; retiring, tombstoned and other networks' labels,
// pending custom hosts and those under an unverified domain serve nothing.

// A dns_label row as a label's allocation or an alias claim leaves it.
func tlsServedTestLabel(ctx context.Context, root string, label string, networkId server.Id, kind string, state string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, $6)`,
			root,
			label,
			networkId,
			kind,
			state,
			server.NowUtc(),
		))
	})
}

// A network_domain row, verified or not.
func tlsServedTestDomain(ctx context.Context, networkId server.Id, domain string, verified bool) {
	var verifyTime *time.Time
	if verified {
		now := server.NowUtc()
		verifyTime = &now
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_domain (network_id, domain, challenge, verify_time) VALUES ($1, $2, $3, $4)`,
			networkId,
			domain,
			"challenge",
			verifyTime,
		))
	})
}

// A network_dns_custom_host row mapping host to an owner label.
func tlsServedTestCustomHost(ctx context.Context, root string, networkId server.Id, host string, owner string, state string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, create_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
			`,
			host,
			root,
			networkId,
			owner,
			state,
			"strong",
			server.NowUtc(),
		))
	})
}

// A listed name as a test compares it.
func tlsServedTestName(name *TlsPublishedName) string {
	expires := ""
	if !name.ExpireTime.IsZero() {
		expires = strconv.FormatInt(name.ExpireTime.UnixMilli(), 10)
	}
	return name.Host + ":" + strconv.Itoa(name.Port) + " label=" + name.Label + " alias=" + name.Alias + " name=" + name.Name +
		" " + name.Audience.String() + " " + expires
}

func TestTlsPublishedNamesOfJoinLabelsAliasesAndCustomHosts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, otherNetworkId := server.NewId(), server.NewId()
		ends := server.NowUtc().Add(time.Hour).Truncate(time.Millisecond)
		public, members := protocol.AclAudience_ACL_AUDIENCE_PUBLIC, protocol.AclAudience_ACL_AUDIENCE_MEMBERS
		grants := []*sidecar.AclPublishedName{
			{Host: "blog", Port: 443, Agreed: true, Audience: public},
			{Host: "shop", Port: 8443, Agreed: true, Audience: members, ExpiresMs: ends.UnixMilli()},
			{Host: "@", Port: 443, Agreed: true, Audience: public},
			{Host: "*-preview", Port: 443, Agreed: true, Audience: members},
		}
		if names := TlsPublishedNamesOf(ctx, networkId, nil); names == nil || len(names) != 0 {
			t.Fatalf("no grants listed %v", names)
		}

		// a network that serves under no name lists each grant once, with none
		bare := TlsPublishedNamesOf(ctx, networkId, grants)
		if len(bare) != len(grants) {
			t.Fatalf("listed %d names of %d grants with no name", len(bare), len(grants))
		}
		for i, name := range bare {
			if name.NetworkId != networkId || name.Host != grants[i].Host || name.Port != grants[i].Port || name.Label != "" ||
				name.Alias != "" || name.Name != "" || name.Audience != grants[i].Audience {
				t.Errorf("a grant with no name: %s", tlsServedTestName(name))
			}
		}
		if !bare[1].ExpireTime.Equal(ends) || !bare[0].ExpireTime.IsZero() {
			t.Errorf("expiries %v and %v", bare[0].ExpireTime, bare[1].ExpireTime)
		}
		if !server.SidecarMg2Linked() {
			return
		}

		root, otherRoot := "ur.example.", "edge.example."
		tlsServedTestLabel(ctx, root, "abcdefghij", networkId, DnsLabelKindLabel, DnsLabelStateLive)
		tlsServedTestLabel(ctx, root, "acme", networkId, DnsLabelKindAlias, DnsLabelStateLive)
		tlsServedTestLabel(ctx, root, "oldname", networkId, DnsLabelKindAlias, DnsLabelStateRetiring)
		tlsServedTestLabel(ctx, otherRoot, "klmnopqrst", networkId, DnsLabelKindLabel, DnsLabelStateLive)
		tlsServedTestLabel(ctx, otherRoot, "gonelabel1", networkId, DnsLabelKindLabel, DnsLabelStateTombstoned)
		tlsServedTestLabel(ctx, root, "uvwxyzabcd", otherNetworkId, DnsLabelKindLabel, DnsLabelStateLive)
		customHosts := server.SidecarMg5Linked() && server.SidecarMg6Linked()
		if customHosts {
			tlsServedTestDomain(ctx, networkId, "brand.example", true)
			tlsServedTestDomain(ctx, networkId, "other.example", false)
			tlsServedTestDomain(ctx, otherNetworkId, "theirs.example", true)
			tlsServedTestCustomHost(ctx, root, networkId, "www.brand.example", "blog", tlsCustomHostStateActive)
			tlsServedTestCustomHost(ctx, root, networkId, "brand.example", "shop", tlsCustomHostStateActive)
			tlsServedTestCustomHost(ctx, root, networkId, "next.brand.example", "blog", "pending")
			tlsServedTestCustomHost(ctx, root, networkId, "www.other.example", "blog", tlsCustomHostStateActive)
			// a name that only ends in a verified domain's text, not at a label boundary
			tlsServedTestCustomHost(ctx, root, networkId, "notbrand.example", "blog", tlsCustomHostStateActive)
			tlsServedTestCustomHost(ctx, root, otherNetworkId, "www.theirs.example", "blog", tlsCustomHostStateActive)
		}

		got := []string{}
		for _, name := range TlsPublishedNamesOf(ctx, networkId, grants) {
			if name.NetworkId != networkId {
				t.Errorf("a name of another network: %s", tlsServedTestName(name))
			}
			got = append(got, tlsServedTestName(name))
		}
		slices.Sort(got)
		endsText := strconv.FormatInt(ends.UnixMilli(), 10)
		want := []string{
			"*-preview:443 label=abcdefghij alias= name=*-preview.abcdefghij.ur.example ACL_AUDIENCE_MEMBERS ",
			"*-preview:443 label=acme alias= name=*-preview.acme.ur.example ACL_AUDIENCE_MEMBERS ",
			"*-preview:443 label=klmnopqrst alias= name=*-preview.klmnopqrst.edge.example ACL_AUDIENCE_MEMBERS ",
			"@:443 label=abcdefghij alias= name=abcdefghij.ur.example ACL_AUDIENCE_PUBLIC ",
			"@:443 label=acme alias= name=acme.ur.example ACL_AUDIENCE_PUBLIC ",
			"@:443 label=klmnopqrst alias= name=klmnopqrst.edge.example ACL_AUDIENCE_PUBLIC ",
			"blog:443 label=abcdefghij alias= name=blog.abcdefghij.ur.example ACL_AUDIENCE_PUBLIC ",
			"blog:443 label=acme alias= name=blog.acme.ur.example ACL_AUDIENCE_PUBLIC ",
			"blog:443 label=klmnopqrst alias= name=blog.klmnopqrst.edge.example ACL_AUDIENCE_PUBLIC ",
			"shop:8443 label=abcdefghij alias= name=shop.abcdefghij.ur.example ACL_AUDIENCE_MEMBERS " + endsText,
			"shop:8443 label=acme alias= name=shop.acme.ur.example ACL_AUDIENCE_MEMBERS " + endsText,
			"shop:8443 label=klmnopqrst alias= name=shop.klmnopqrst.edge.example ACL_AUDIENCE_MEMBERS " + endsText,
		}
		if customHosts {
			want = append(want,
				"blog:443 label= alias=www.brand.example name=www.brand.example ACL_AUDIENCE_PUBLIC ",
				"shop:8443 label= alias=brand.example name=brand.example ACL_AUDIENCE_MEMBERS "+endsText,
			)
		}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("listed\n%v\nwant\n%v", got, want)
		}
	})
}
