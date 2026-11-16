//go:build sidecar_pending

package model

// The names certs' order route builds a network's identifier set from
// (tls_cert_names.go; QD-846): no label and nothing written without allocate,
// the label §12.5 gives with it, and the live aliases alone beside it.

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

func TestGetNetworkTlsCertNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg2Linked() {
			return
		}
		ctx := context.Background()
		const root = "certs-names.example"
		networkId := server.NewId()

		label, aliases, err := GetNetworkTlsCertNames(ctx, networkId, root, false)
		if err != nil || label != "" || len(aliases) != 0 {
			t.Fatalf("a network with no label: %q, %v, %v", label, aliases, err)
		}
		if GetNetworkLabel(ctx, networkId, root) != "" {
			t.Fatal("a read without allocate gave the network a label")
		}

		label, aliases, err = GetNetworkTlsCertNames(ctx, networkId, root, true)
		if err != nil || label == "" || len(aliases) != 0 {
			t.Fatalf("the first allocating read: %q, %v, %v", label, aliases, err)
		}
		if stored := GetNetworkLabel(ctx, networkId, root); stored != label {
			t.Fatalf("the label read back is %q, want %q", stored, label)
		}

		// a live alias is named; a retiring one, another network's and another root's are not
		hold := func(holdRoot string, name string, holder server.Id, state string) {
			t.Helper()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, $6)`,
					dnsLabelRoot(holdRoot),
					name,
					holder,
					DnsLabelKindAlias,
					state,
					server.NowUtc(),
				))
			})
		}
		suffix := networkId.String()[:8]
		hold(root, "live-"+suffix, networkId, DnsLabelStateLive)
		hold(root, "also-"+suffix, networkId, DnsLabelStateLive)
		hold(root, "gone-"+suffix, networkId, DnsLabelStateRetiring)
		hold(root, "theirs-"+suffix, server.NewId(), DnsLabelStateLive)
		hold("other-"+root, "elsewhere-"+suffix, networkId, DnsLabelStateLive)
		for _, allocate := range []bool{false, true} {
			again, aliases, err := GetNetworkTlsCertNames(ctx, networkId, root+".", allocate)
			if err != nil || again != label {
				t.Fatalf("allocate %t: the label is %q, want %q: %v", allocate, again, label, err)
			}
			if !slices.Equal(aliases, []string{"also-" + suffix, "live-" + suffix}) {
				t.Fatalf("allocate %t: the aliases are %v", allocate, aliases)
			}
		}
	})
}
