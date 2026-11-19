package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
)

// The label map's rows as the engine routes by them: an ownerless name with
// the zero network, a suspension only with its reason, its end in UTC, and
// the withheld owners relative to each name.
func TestTlsEdgeNameMapConvertsTheLabelMap(t *testing.T) {
	networkId := server.NewId()
	until := time.Date(2026, time.March, 4, 5, 6, 7, 0, time.FixedZone("synthetic", 3600))
	root := "sidecar.example."
	owners := []string{
		"abcdefghij." + root, "shop.abcdefghij." + root, "a.shop.abcdefghij." + root, "shop.acme." + root,
		"xabcdefghij." + root, "shop.abcdefghij.other.example.",
	}
	nameMap := tlsEdgeNameMap(&DnsLabelMap{
		Root:          root,
		Serial:        41,
		RestoreSerial: 7,
		Entries: []*DnsLabelMapEntry{
			{Root: root, Label: "abcdefghij", NetworkId: &networkId, Kind: DnsLabelKindLabel, State: DnsLabelStateLive, WithheldHosts: owners},
			{Root: root, Label: "acme", NetworkId: &networkId, Kind: DnsLabelKindAlias, State: DnsLabelStateRetiring, SuspendReason: "abuse", SuspendUntil: &until, WithheldHosts: owners},
			{Root: root, Label: "klmnopqrst", Kind: DnsLabelKindLabel, State: DnsLabelStateTombstoned, SuspendUntil: &until},
		},
	})
	if nameMap.Serial != 41 || nameMap.RestoreSerial != 7 || len(nameMap.Entries) != 3 {
		t.Fatalf("map %+v", nameMap)
	}
	label := nameMap.Entries[0]
	if label.Name != "abcdefghij" || [16]byte(label.NetworkId) != [16]byte(networkId) || label.Kind != tlsedgeservice.TlsEdgeNameKindLabel || label.State != tlsedgeservice.TlsEdgeNameStateLive {
		t.Errorf("label %+v", label)
	}
	if label.ZoneSuspended || !label.ZoneSuspendUntil.IsZero() || !slices.Equal(label.WithheldHosts, []string{"shop", "a.shop"}) {
		t.Errorf("label's suspensions %+v", label)
	}
	alias := nameMap.Entries[1]
	if alias.Kind != tlsedgeservice.TlsEdgeNameKindAlias || alias.State != tlsedgeservice.TlsEdgeNameStateRetiring || !alias.ZoneSuspended ||
		!alias.ZoneSuspendUntil.Equal(until) || alias.ZoneSuspendUntil.Location() != time.UTC || !slices.Equal(alias.WithheldHosts, []string{"shop"}) {
		t.Errorf("alias %+v", alias)
	}
	ownerless := nameMap.Entries[2]
	if [16]byte(ownerless.NetworkId) != [16]byte{} || ownerless.State != tlsedgeservice.TlsEdgeNameStateTombstoned || ownerless.ZoneSuspended || !ownerless.ZoneSuspendUntil.IsZero() || len(ownerless.WithheldHosts) != 0 {
		t.Errorf("an ownerless name %+v", ownerless)
	}
}

// A binary that links no sidecar migration answers every read with an error,
// so the engine stays not ready and never serves on an empty map.
func TestTlsEdgeStoreAnswersErrorsUnlinked(t *testing.T) {
	if server.SidecarMg2Linked() {
		t.Skip("this binary links the label map")
	}
	store := NewTlsEdgeStore(nil)
	ctx := context.Background()
	if _, err := store.NameSnapshot(ctx, "sidecar.example"); err == nil {
		t.Error("a snapshot with no label map")
	}
	if _, err := store.NamesSince(ctx, "sidecar.example", 0); err == nil {
		t.Error("changes with no label map")
	}
	if _, err := store.Suspensions(ctx); err == nil {
		t.Error("suspensions with no table")
	}
	if err := store.InsertConnectionRecords(ctx, []*tlsedgeservice.TlsConnectionRecord{{}}); err == nil {
		t.Error("records with no table")
	}
}

// The active custom hosts as the engine routes by them (TLS-M6-02): each owner cut into its host and its
// network's label, and a row of another root, a strict-mode MX host and an owner that is no one host of a
// label's zone left out.
func TestTlsEdgeCustomHostEntriesCutTheOwner(t *testing.T) {
	networkId := server.NewId()
	root := "sidecar.example."
	rows := []*DnsCustomHostRow{
		{Host: "app.corp.example", Root: root, NetworkId: networkId, Owner: "web.abcdefghij." + root, State: "active"},
		{Host: "corp.example", Root: root, NetworkId: networkId, Owner: "shop.abcdefghij." + root, State: "active"},
		{Host: "other.corp.example", Root: "other.example.", NetworkId: networkId, Owner: "web.abcdefghij.other.example.", State: "active"},
		{Host: "mail.corp.example", Root: root, NetworkId: networkId, Owner: "mx.abcdefghij." + root, State: "active"},
		{Host: "apex.corp.example", Root: root, NetworkId: networkId, Owner: "abcdefghij." + root, State: "active"},
		{Host: "deep.corp.example", Root: root, NetworkId: networkId, Owner: "a.b.abcdefghij." + root, State: "active"},
	}
	entries := tlsEdgeCustomHostEntries(rows, "sidecar.example")
	if len(entries) != 2 {
		t.Fatalf("%d entries: %+v", len(entries), entries)
	}
	for index, want := range []tlsedgeservice.TlsEdgeCustomHostEntry{
		{Host: "app.corp.example", OwnerHost: "web", OwnerLabel: "abcdefghij"},
		{Host: "corp.example", OwnerHost: "shop", OwnerLabel: "abcdefghij"},
	} {
		entry := entries[index]
		if entry.Host != want.Host || entry.OwnerHost != want.OwnerHost || entry.OwnerLabel != want.OwnerLabel || [16]byte(entry.NetworkId) != [16]byte(networkId) {
			t.Errorf("entry %d: %+v", index, entry)
		}
	}
}
