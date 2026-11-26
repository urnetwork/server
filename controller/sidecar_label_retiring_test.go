//go:build sidecar_pending

package controller

// GET /sidecar/label's retiring aliases (§12.5's alias and retirement rows, D-DNS-10) on the private test stack, with
// MG2: the answer names each alias the network changed or released with the time its 90 days end, the window the
// alias routes answer and the 90-day job tombstones by, so a sidecar's label table keeps it until then; a tombstoned
// alias, another network's retiring alias and the live alias are not among them; and a network with none answers
// none, which the body leaves out.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestSidecarLabelAnswersTheRetiringAliasesWithTheirEnds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testSidecarLabelRoot()
		defer model.Testing_SetDnsUserRoot(root)()
		networkId, network := testSidecarLabelNetwork(t, ctx)
		otherNetworkId, _ := testSidecarLabelNetwork(t, ctx)
		clientSession := testSidecarLabelClientSession(ctx, network)

		none, err := GetSidecarLabel(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if none.RetiringAliases != nil {
			t.Fatalf("a network with no alias answered retiring aliases %+v", none.RetiringAliases)
		}
		body, err := json.Marshal(none)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "retiring_aliases") {
			t.Fatalf("a network with no retiring alias answered %s", body)
		}

		insertAlias := func(label string, holderId server.Id, state string, stateTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, $6)`,
					root+".",
					label,
					holderId,
					model.DnsLabelKindAlias,
					state,
					stateTime,
				))
			})
		}
		suffix := strings.ToLower(strings.ReplaceAll(networkId.String(), "-", "")[20:])
		changed := server.NowUtc().Add(-24 * time.Hour).Truncate(time.Millisecond)
		released := changed.Add(-30 * 24 * time.Hour)
		insertAlias("team-"+suffix, networkId, model.DnsLabelStateLive, server.NowUtc())
		insertAlias("old-"+suffix, networkId, model.DnsLabelStateRetiring, changed)
		insertAlias("older-"+suffix, networkId, model.DnsLabelStateRetiring, released)
		insertAlias("gone-"+suffix, networkId, model.DnsLabelStateTombstoned, released)
		insertAlias("other-"+suffix, otherNetworkId, model.DnsLabelStateRetiring, changed)

		answer, err := GetSidecarLabel(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		window := model.DefaultDnsAliasSettings().RetiringWindow
		want := []sidecar.NetworkLabelRetiringAlias{
			{Alias: "old-" + suffix, TombstoneTime: changed.Add(window)},
			{Alias: "older-" + suffix, TombstoneTime: released.Add(window)},
		}
		if answer.Alias != "team-"+suffix || answer.Label != none.Label || len(answer.RetiringAliases) != len(want) {
			t.Fatalf("the answer %+v, want the live alias and %+v", answer, want)
		}
		for index, retiring := range answer.RetiringAliases {
			if retiring.Alias != want[index].Alias || !retiring.TombstoneTime.Equal(want[index].TombstoneTime) {
				t.Fatalf("retiring alias %d answered %+v, want %+v", index, retiring, want[index])
			}
		}

		// what a sidecar reads: the body, which its table takes the ends from
		body, err = json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		read := &sidecar.SidecarLabel{}
		if err := json.Unmarshal(body, read); err != nil {
			t.Fatal(err)
		}
		if len(read.RetiringAliases) != len(want) || !read.RetiringAliases[0].TombstoneTime.Equal(want[0].TombstoneTime) {
			t.Fatalf("the body %s reads back as %+v", body, read)
		}
	})
}
