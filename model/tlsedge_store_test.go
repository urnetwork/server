package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
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

// Whatever a store call raises comes back from tlsEdgeStoreRun as an error and never leaves it, since its callers
// are tlsedge's own goroutines, with no server handler to catch a raise (QD-1090): server.Db's done error at a context
// that ended under the call, which it raised again before, answers as an error that is still the context's, as does
// connect's "Done"; an ordinary error answers as itself and any other value as its text, as before; and a call that
// raised nothing answers nil.
func TestTlsEdgeStoreRunAnswersEveryRaiseAsAnError(t *testing.T) {
	storeErr := errors.New("synthetic store failure")
	// what server.Db raises when its context ends under it: the done sentinel joined to the acquire's and the
	// context's errors
	doneErr := errors.Join(server.DbContextDoneError, context.Canceled, context.Canceled)
	if !server.IsDoneError(doneErr) {
		t.Fatal("the synthetic stop is not a done error, so this test would prove nothing of one")
	}
	for _, c := range []struct {
		raised any
		// the answer's text, and an error it must wrap where there is one
		text string
		is   error
	}{
		{raised: doneErr, text: doneErr.Error(), is: context.Canceled},
		{raised: "Done", text: "Done"},
		{raised: storeErr, text: storeErr.Error(), is: storeErr},
		{raised: 42, text: "42"},
	} {
		reraised, err := func() (reraised any, err error) {
			defer func() {
				reraised = recover()
			}()
			return nil, tlsEdgeStoreRun(func() {
				panic(c.raised)
			})
		}()
		if reraised != nil {
			t.Errorf("a raise of %v left the store's run: %v", c.raised, reraised)
		} else if err == nil || err.Error() != c.text || (c.is != nil && !errors.Is(err, c.is)) {
			t.Errorf("a raise of %v answered %v", c.raised, err)
		}
	}
	if err := tlsEdgeStoreRun(func() {}); err != nil {
		t.Errorf("a call that raised nothing answered %v", err)
	}
}

// Every call whose context ended, as the engine's StoreTimeout or its close ends one on a slow Postgres, answers the
// context's error, a context past its deadline the deadline's, and never raises into its caller, one of tlsedge's
// own goroutines with no server handler to catch it (QD-1090); the store answers again once a context lives. Before
// QD-1090 each call raised server.Db's done error at a canceled context, which ended the tlsedge process.
func TestTlsEdgeStoreAnswersAnEndedContextWithAnError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg6Linked() {
			return
		}
		ctx := context.Background()
		root := "sidecar.example."
		store := NewTlsEdgeStore(nil)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		expired, expire := context.WithDeadline(ctx, server.NowUtc().Add(-time.Minute))
		defer expire()
		now := server.NowUtc()
		// a record a row can hold, of an hour neither retired nor ahead, so the write reaches Postgres
		record := &tlsedgeservice.TlsConnectionRecord{
			ConnectionId: sidecar.NewId(),
			NetworkId:    sidecar.NewId(),
			Host:         "shop",
			StartTime:    now.Add(-time.Minute),
			EndTime:      now,
			ByteCount:    1024,
			VisitorHash:  make([]byte, 16),
		}
		calls := map[string]func(ctx context.Context) error{
			"NameSnapshot": func(ctx context.Context) error {
				_, err := store.NameSnapshot(ctx, root)
				return err
			},
			"NamesSince": func(ctx context.Context) error {
				_, err := store.NamesSince(ctx, root, 0)
				return err
			},
			"CustomHosts": func(ctx context.Context) error {
				_, err := store.CustomHosts(ctx, root)
				return err
			},
			"Suspensions": func(ctx context.Context) error {
				_, err := store.Suspensions(ctx)
				return err
			},
			"InsertConnectionRecords": func(ctx context.Context) error {
				return store.InsertConnectionRecords(ctx, []*tlsedgeservice.TlsConnectionRecord{record})
			},
		}
		for _, ended := range []struct {
			ctx context.Context
			err error
		}{
			{ctx: canceled, err: context.Canceled},
			{ctx: expired, err: context.DeadlineExceeded},
		} {
			for name, call := range calls {
				raised, err := func() (raised any, err error) {
					defer func() {
						raised = recover()
					}()
					return nil, call(ended.ctx)
				}()
				if raised != nil {
					t.Errorf("%s on a context ended by %v raised %v", name, ended.err, raised)
				} else if !errors.Is(err, ended.err) {
					t.Errorf("%s on a context ended by %v answered %v", name, ended.err, err)
				}
			}
		}
		if _, err := store.NameSnapshot(ctx, root); err != nil {
			t.Fatalf("a snapshot after the ended calls: %v", err)
		}
		if _, err := store.Suspensions(ctx); err != nil {
			t.Fatalf("the suspensions after the ended calls: %v", err)
		}
	})
}
