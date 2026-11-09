//go:build sidecar_pending

package model

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A reader whose every draw gives the same label, so two allocations collide.
type repeatingDnsLabelReader struct {
	draw []byte
}

func (self *repeatingDnsLabelReader) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = self.draw[index%len(self.draw)]
	}
	return len(p), nil
}

// Draws one fixed label first, then fresh ones from crypto/rand.
type firstDnsLabelReader struct {
	stateLock sync.Mutex
	first     io.Reader
	rest      io.Reader
}

func (self *firstDnsLabelReader) Read(p []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.first != nil {
		n, err := io.ReadFull(self.first, p)
		self.first = nil
		return n, err
	}
	return self.rest.Read(p)
}

// The bytes NewDnsLabel reads to draw label: each byte indexes its alphabet directly.
func testDnsLabelDrawBytes(t testing.TB, label string) []byte {
	t.Helper()
	draw := []byte{}
	for index, character := range []byte(label) {
		switch {
		case index == 0:
			draw = append(draw, character-'a')
		case 'a' <= character && character <= 'z':
			draw = append(draw, character-'a')
		default:
			draw = append(draw, 26+character-'0')
		}
	}
	got, err := sidecar.NewDnsLabel(bytes.NewReader(append(draw, bytes.Repeat([]byte{0}, 64)...)))
	if err != nil || got != label {
		t.Fatalf("the draw bytes give %q, %v", got, err)
	}
	return draw
}

// Two networks whose first draws collide: the second redraws and gets a label of its own, and a
// network's second need returns its first label.
func TestDnsLabelCollisionRedraws(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := "labels-" + server.NewId().String()[:8] + ".example."
		collide := testDnsLabelDrawBytes(t, "kcollided0")
		restore := Testing_SetDnsLabelRandom(&firstDnsLabelReader{first: bytes.NewReader(append(collide, bytes.Repeat([]byte{0}, 64)...)), rest: &repeatingDnsLabelReader{draw: collide}})
		firstNetworkId := server.NewId()
		first, err := EnsureNetworkLabel(ctx, firstNetworkId, root)
		restore()
		if err != nil || first != "kcollided0" {
			t.Fatalf("first %q, %v", first, err)
		}
		// every draw now collides until the reader changes
		restore = Testing_SetDnsLabelRandom(&repeatingDnsLabelReader{draw: collide})
		if _, err := EnsureNetworkLabel(ctx, server.NewId(), root); !errors.Is(err, ErrDnsLabelDrawsSpent) {
			restore()
			t.Fatalf("a run of collisions answered %v", err)
		}
		restore()
		restore = Testing_SetDnsLabelRandom(&firstDnsLabelReader{first: bytes.NewReader(append(collide, bytes.Repeat([]byte{0}, 64)...)), rest: randReader()})
		defer restore()
		second, err := EnsureNetworkLabel(ctx, server.NewId(), root)
		if err != nil || second == first || len(second) != 10 {
			t.Fatalf("second %q, %v", second, err)
		}
		again, err := EnsureNetworkLabel(ctx, firstNetworkId, root)
		if err != nil || again != first {
			t.Fatalf("a second need gave %q, %v", again, err)
		}
	})
}

// A draw the screen refuses is drawn again (owner decision 97).
func TestDnsLabelScreenRedraws(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := "screen-" + server.NewId().String()[:8] + ".example."
		refused := map[string]bool{}
		var stateLock sync.Mutex
		restore := SetDnsNameScreen(func(label string) bool {
			stateLock.Lock()
			defer stateLock.Unlock()
			if len(refused) == 0 {
				refused[label] = true
				return false
			}
			return true
		})
		defer restore()
		label, err := EnsureNetworkLabel(ctx, server.NewId(), root)
		if err != nil {
			t.Fatal(err)
		}
		if refused[label] || len(refused) != 1 {
			t.Fatalf("label %q after refusing %v", label, refused)
		}
	})
}

// D-DNS-2's order: label, live alias, retiring alias, and a legacy name only when interactive. A
// tombstone and an ownerless row resolve to nothing.
func TestResolveNetworkLabelOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := "resolve-" + server.NewId().String()[:8] + ".example."
		networkId := server.NewId()
		legacyName := "legacy-" + server.NewId().String()[:8]
		Testing_CreateNetwork(ctx, networkId, legacyName, server.NewId())
		label, err := EnsureNetworkLabel(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		otherNetworkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, row := range []struct {
				label     string
				networkId *server.Id
				kind      string
				state     string
			}{
				{label: "live-alias", networkId: &networkId, kind: DnsLabelKindAlias, state: DnsLabelStateLive},
				{label: "old-alias", networkId: &networkId, kind: DnsLabelKindAlias, state: DnsLabelStateRetiring},
				{label: "gone-alias", networkId: &otherNetworkId, kind: DnsLabelKindAlias, state: DnsLabelStateTombstoned},
				{label: "ownerless", networkId: nil, kind: DnsLabelKindAlias, state: DnsLabelStateRetiring},
			} {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, now())
				`, root, row.label, row.networkId, row.kind, row.state))
			}
		})
		cases := []struct {
			text        string
			interactive bool
			kind        sidecar.NetworkLabelKind
		}{
			{text: label, kind: sidecar.NetworkLabelKindLabel},
			{text: label + "." + root, kind: sidecar.NetworkLabelKindLabel},
			{text: "LIVE-ALIAS", kind: sidecar.NetworkLabelKindAlias},
			{text: "old-alias", kind: sidecar.NetworkLabelKindRetiringAlias},
			{text: legacyName, interactive: true, kind: sidecar.NetworkLabelKindLegacyName},
		}
		for _, c := range cases {
			resolvedId, kind, err := ResolveNetworkLabel(ctx, root, c.text, c.interactive)
			if err != nil || resolvedId != networkId || kind != c.kind {
				t.Errorf("%q: %v %s %v", c.text, resolvedId, kind, err)
			}
		}
		for _, c := range []struct {
			text        string
			interactive bool
		}{
			{text: legacyName},
			{text: "gone-alias", interactive: true},
			{text: "ownerless", interactive: true},
			{text: "nothing-here", interactive: true},
		} {
			if _, _, err := ResolveNetworkLabel(ctx, root, c.text, c.interactive); !errors.Is(err, sidecar.ErrNetworkLabelUnknown) {
				t.Errorf("%q resolved: %v", c.text, err)
			}
		}
	})
}

// A name dns_label holds in any state is held, and an alias cannot take a label's name, nor the
// reverse, live or not.
func TestDnsNameHeldAcrossStates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := "held-" + server.NewId().String()[:8] + ".example."
		networkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for index, state := range []string{DnsLabelStateLive, DnsLabelStateRetiring, DnsLabelStateTombstoned} {
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, now())
				`, root, "held-"+string(rune('a'+index)), networkId, DnsLabelKindLabel, state))
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			for index := range 3 {
				name := "held-" + string(rune('a'+index))
				if !DnsNameHeldInTx(ctx, tx, name) || !(dnsLabelHolder{}).HoldsNameInTx(ctx, tx, name) {
					t.Errorf("%s is not held", name)
				}
				tag := server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, now())
					ON CONFLICT (root, label) DO NOTHING
				`, root, name, server.NewId(), DnsLabelKindAlias, DnsLabelStateLive))
				if tag.RowsAffected() != 0 {
					t.Errorf("an alias took %s", name)
				}
			}
			if DnsNameHeldInTx(ctx, tx, "held-z") {
				t.Error("a name no row holds is held")
			}
		})
	})
}

// Deleting a network withdraws its records in the deletion's commit, through one journal row, and
// tombstones its label without freeing it.
func TestDeletingANetworkWithdrawsItsRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "withdraw-"+server.NewId().String()[:8], userId)
		label, err := EnsureNetworkLabel(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		apex := label + "." + root
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+apex, "one"), testDnsTxt("b."+apex, "two")}}); err != nil {
			t.Fatal(err)
		}
		if success, _ := RemoveNetwork(ctx, networkId, &userId); !success {
			t.Fatal("the network was not removed")
		}
		rows := testDnsJournal(t, ctx, root, 1)
		if len(rows) != 1 || len(rows[0].Deletes) != 2 || len(rows[0].Upserts) != 0 {
			t.Fatalf("the withdrawal's journal %+v", rows)
		}
		snapshot, err := OpenDnsSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		defer snapshot.Close()
		if set, err := snapshot.Next(); err != io.EOF {
			t.Fatalf("a withdrawn record is still served: %+v %v", set, err)
		}
		if _, _, err := ResolveNetworkLabel(ctx, root, label, true); !errors.Is(err, sidecar.ErrNetworkLabelUnknown) {
			t.Fatalf("a deleted network's label resolves: %v", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			if !DnsNameHeldInTx(ctx, conn, label) {
				t.Fatal("the deletion freed the label")
			}
		})
	})
}

// The label map's deltas come in change serial order, each change at the serial of the write that made
// it, with the zone's suspension beside each name.
func TestDnsLabelMapDeltasInSerialOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		firstNetworkId, firstApex := testDnsNetwork(t, ctx, root)
		secondNetworkId, secondApex := testDnsNetwork(t, ctx, root)
		labelMap, err := DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if len(labelMap.Entries) != 2 || labelMap.Serial != 0 {
			t.Fatalf("snapshot %+v", labelMap)
		}
		var serials []uint64
		for _, change := range []struct {
			networkId server.Id
			apex      string
		}{{firstNetworkId, firstApex}, {secondNetworkId, secondApex}, {firstNetworkId, firstApex}} {
			var serial uint64
			server.Tx(ctx, func(tx server.PgTx) {
				result, err := DnsWriteInTx(ctx, tx, &DnsWriteArgs{Root: root, NetworkId: change.networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+change.apex, "x")}})
				if err != nil {
					t.Fatal(err)
				}
				serial = result.Serial
				touchDnsLabelMapInTx(ctx, tx, root, change.networkId, serial)
			}, server.TxReadCommitted)
			serials = append(serials, serial)
		}
		since, err := DnsLabelMapSince(ctx, root, serials[0])
		if err != nil {
			t.Fatal(err)
		}
		if len(since.Entries) != 2 || since.Serial != serials[2] {
			t.Fatalf("since %d: %+v", serials[0], since)
		}
		if since.Entries[0].ChangeSerial != serials[1] || since.Entries[1].ChangeSerial != serials[2] {
			t.Fatalf("out of order: %+v %+v", since.Entries[0], since.Entries[1])
		}
		nothing, err := DnsLabelMapSince(ctx, root, since.Serial)
		if err != nil || len(nothing.Entries) != 0 || nothing.Serial != since.Serial {
			t.Fatalf("an empty read: %+v %v", nothing, err)
		}
	})
}

func randReader() io.Reader {
	_, random := currentDnsLabelDeps()
	return random
}
