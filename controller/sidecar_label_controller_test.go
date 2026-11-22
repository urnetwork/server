//go:build sidecar_pending

package controller

// GET /sidecar/label's controller (§12.5; owner decision 421, QD-935) on the private test stack, with MG2: a first
// request allocates the network's label and every later one answers it again drawing nothing; two first requests at
// once, held together just before the label lock, allocate one label; the live alias is answered and a retiring or
// tombstoned one is not; with no dns root the route answers 503 and allocates nothing.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// §12.5's label: 10 characters, a letter and then letters or digits.
var testSidecarLabelPattern = regexp.MustCompile(`^[a-z][a-z0-9]{9}$`)

// A random source every read of fails, so a draw made while it is in place answers an error.
type failingSidecarLabelReader struct{}

func (self failingSidecarLabelReader) Read(p []byte) (int, error) {
	return 0, errors.New("no label may be drawn here")
}

// A dns root no other test serves, as dns.yml names one.
func testSidecarLabelRoot() string {
	id := strings.ReplaceAll(server.NewId().String(), "-", "")
	return "label-" + id[len(id)-12:] + ".example"
}

// A new network and a session of one of its clients, as the router's wrapper leaves it.
func testSidecarLabelNetwork(t testing.TB, ctx context.Context) (server.Id, *session.ByJwt) {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	id := strings.ReplaceAll(networkId.String(), "-", "")
	networkName := "labelnet" + id[len(id)-12:]
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	return networkId, session.NewByJwt(networkId, userId, networkName, false, false)
}

func testSidecarLabelClientSession(ctx context.Context, network *session.ByJwt) *session.ClientSession {
	return session.Testing_CreateClientSession(ctx, network.Client(server.NewId(), server.NewId()))
}

// The network's label rows in root, of any state, as dns_label holds them.
func testSidecarLabelRows(ctx context.Context, networkId server.Id, root string) []string {
	labels := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT label FROM dns_label WHERE network_id = $1 AND root = $2 AND kind = $3 ORDER BY label`,
			networkId,
			root+".",
			model.DnsLabelKindLabel,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var label string
				server.Raise(result.Scan(&label))
				labels = append(labels, label)
			}
		})
	})
	return labels
}

// The first request allocates one label of §12.5's form for the token's network and leaves out the alias it does
// not have; with every draw failing after that, a second request, another client's and the network credential's
// answer the same label, so none of them drew, while a new network's first request, which must draw, fails.
func TestSidecarLabelAllocatesOnFirstNeedAndThenAnswersIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testSidecarLabelRoot()
		defer model.Testing_SetDnsUserRoot(root)()
		networkId, network := testSidecarLabelNetwork(t, ctx)
		clientSession := testSidecarLabelClientSession(ctx, network)
		if rows := testSidecarLabelRows(ctx, networkId, root); len(rows) != 0 {
			t.Fatalf("a new network holds labels %v", rows)
		}

		first, err := GetSidecarLabel(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if first.NetworkId != networkId || !testSidecarLabelPattern.MatchString(first.Label) || first.Alias != "" {
			t.Fatalf("the first answer %+v", first)
		}
		if rows := testSidecarLabelRows(ctx, networkId, root); !slices.Equal(rows, []string{first.Label}) {
			t.Fatalf("the first request left labels %v, answered %q", rows, first.Label)
		}

		defer model.Testing_SetDnsLabelRandom(failingSidecarLabelReader{})()
		for _, caller := range []*session.ClientSession{
			clientSession,
			testSidecarLabelClientSession(ctx, network),
			session.Testing_CreateClientSession(ctx, network),
		} {
			again, err := GetSidecarLabel(caller)
			if err != nil {
				t.Fatalf("a later request drew: %v", err)
			}
			if *again != *first {
				t.Fatalf("a later request answered %+v, the first %+v", again, first)
			}
		}
		if rows := testSidecarLabelRows(ctx, networkId, root); !slices.Equal(rows, []string{first.Label}) {
			t.Fatalf("later requests left labels %v", rows)
		}

		// the failing source does fail a draw: a network with no label gets none
		otherNetworkId, otherNetwork := testSidecarLabelNetwork(t, ctx)
		if answer, err := GetSidecarLabel(testSidecarLabelClientSession(ctx, otherNetwork)); err == nil {
			t.Fatalf("a draw from the failing source answered %+v", answer)
		}
		if rows := testSidecarLabelRows(ctx, otherNetworkId, root); len(rows) != 0 {
			t.Fatalf("a failed draw left labels %v", rows)
		}
	})
}

// Two first requests of one network, held together just before the label lock with neither having read the table,
// so only the lock stands between them and two labels: both answer one label, and the table holds one.
func TestSidecarLabelTwoFirstRequestsAllocateOneLabel(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testSidecarLabelRoot()
		defer model.Testing_SetDnsUserRoot(root)()
		networkId, network := testSidecarLabelNetwork(t, ctx)

		arrived := make(chan struct{}, 2)
		release := make(chan struct{})
		model.Testing_DnsLabelLockBarrier = func(barrierNetworkId server.Id) {
			if barrierNetworkId != networkId {
				return
			}
			arrived <- struct{}{}
			<-release
		}
		t.Cleanup(func() { model.Testing_DnsLabelLockBarrier = nil })

		type labelAnswer struct {
			label *sidecar.SidecarLabel
			err   error
		}
		answers := make(chan labelAnswer, 2)
		for range 2 {
			caller := testSidecarLabelClientSession(ctx, network)
			go func() {
				label, err := GetSidecarLabel(caller)
				answers <- labelAnswer{label: label, err: err}
			}()
		}
		for range 2 {
			select {
			case <-arrived:
			case answer := <-answers:
				close(release)
				t.Fatalf("a request answered without reaching the label lock: %+v, %v", answer.label, answer.err)
			}
		}
		close(release)
		first := <-answers
		second := <-answers
		if first.err != nil || second.err != nil {
			t.Fatalf("the requests: %v, %v", first.err, second.err)
		}
		if *first.label != *second.label || !testSidecarLabelPattern.MatchString(first.label.Label) {
			t.Fatalf("two first requests answered %+v and %+v", first.label, second.label)
		}
		if rows := testSidecarLabelRows(ctx, networkId, root); !slices.Equal(rows, []string{first.label.Label}) {
			t.Fatalf("two first requests left labels %v", rows)
		}
	})
}

// The network's live alias is answered beside its label; a retiring alias, a tombstoned one and another network's
// live alias are not.
func TestSidecarLabelAnswersTheLiveAliasAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testSidecarLabelRoot()
		defer model.Testing_SetDnsUserRoot(root)()
		networkId, network := testSidecarLabelNetwork(t, ctx)
		otherNetworkId, _ := testSidecarLabelNetwork(t, ctx)
		clientSession := testSidecarLabelClientSession(ctx, network)
		insertAlias := func(label string, holderId server.Id, state string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, now())`,
					root+".",
					label,
					holderId,
					model.DnsLabelKindAlias,
					state,
				))
			})
		}
		suffix := strings.ToLower(strings.ReplaceAll(networkId.String(), "-", "")[20:])
		insertAlias("old-"+suffix, networkId, model.DnsLabelStateRetiring)
		insertAlias("gone-"+suffix, networkId, model.DnsLabelStateTombstoned)
		insertAlias("other-"+suffix, otherNetworkId, model.DnsLabelStateLive)

		withoutLive, err := GetSidecarLabel(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if withoutLive.Alias != "" || withoutLive.NetworkId != networkId {
			t.Fatalf("with no live alias: %+v", withoutLive)
		}

		insertAlias("team-"+suffix, networkId, model.DnsLabelStateLive)
		withLive, err := GetSidecarLabel(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if withLive.Alias != "team-"+suffix || withLive.Label != withoutLive.Label || withLive.NetworkId != networkId {
			t.Fatalf("with a live alias: %+v, before %+v", withLive, withoutLive)
		}
	})
}

// With no dns root the route answers 503 unavailable, never a label of another root, and allocates nothing.
func TestSidecarLabelRefusesWithoutARoot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetDnsUserRoot("")()
		networkId, network := testSidecarLabelNetwork(t, ctx)
		answer, err := GetSidecarLabel(testSidecarLabelClientSession(ctx, network))
		var apiErr *sidecar.DnsApiError
		if answer != nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != sidecar.DnsApiCodeUnavailable {
			t.Fatalf("with no root: %+v, %v", answer, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM dns_label WHERE network_id = $1`, networkId).Scan(&count))
			if count != 0 {
				t.Fatalf("with no root the network holds %d names", count)
			}
		})
	})
}

// In a root that takes serials, the route's first draw writes the label's zone with it, so the label comes back
// from a read of the label map since the cursor a tlsedge held before the request, at the zone's serial: a label
// left at serial 0 there was never routed by a running tlsedge.
func TestSidecarLabelDrawTakesItsPlaceInTheLabelMap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		root := testSidecarLabelRoot()
		defer model.Testing_SetDnsUserRoot(root)()
		if err := model.EnsureDnsRoot(ctx, root+"."); err != nil {
			t.Fatal(err)
		}
		cursor, err := model.DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		networkId, network := testSidecarLabelNetwork(t, ctx)
		answer, err := GetSidecarLabel(testSidecarLabelClientSession(ctx, network))
		if err != nil {
			t.Fatal(err)
		}
		since, err := model.DnsLabelMapSince(ctx, root, cursor.Serial)
		if err != nil {
			t.Fatal(err)
		}
		if len(since.Entries) != 1 || since.Entries[0].Label != answer.Label || since.Entries[0].NetworkId == nil || *since.Entries[0].NetworkId != networkId || since.Entries[0].ChangeSerial <= cursor.Serial {
			t.Fatalf("the read since %d after the draw of %q: %+v", cursor.Serial, answer.Label, since.Entries)
		}
	})
}

// While the root is restoring, a first request answers 503 unavailable and allocates nothing, since the draw would
// write the label's zone and a restoring root refuses every write (§12.6); a network that has its label is answered
// it, since that draws nothing.
func TestSidecarLabelWhileRestoringRefusesAFirstDraw(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		root := testSidecarLabelRoot()
		defer model.Testing_SetDnsUserRoot(root)()
		if err := model.EnsureDnsRoot(ctx, root+"."); err != nil {
			t.Fatal(err)
		}
		_, labeledNetwork := testSidecarLabelNetwork(t, ctx)
		labeled, err := GetSidecarLabel(testSidecarLabelClientSession(ctx, labeledNetwork))
		if err != nil {
			t.Fatal(err)
		}
		if err := model.SetDnsRestoring(ctx, root+".", true); err != nil {
			t.Fatal(err)
		}
		networkId, network := testSidecarLabelNetwork(t, ctx)
		answer, err := GetSidecarLabel(testSidecarLabelClientSession(ctx, network))
		var apiErr *sidecar.DnsApiError
		if answer != nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != sidecar.DnsApiCodeUnavailable {
			t.Fatalf("a first draw while restoring: %+v, %v", answer, err)
		}
		if rows := testSidecarLabelRows(ctx, networkId, root); len(rows) != 0 {
			t.Fatalf("a refused draw left labels %v", rows)
		}
		again, err := GetSidecarLabel(testSidecarLabelClientSession(ctx, labeledNetwork))
		if err != nil || *again != *labeled {
			t.Fatalf("a labeled network while restoring: %+v, %v; before %+v", again, err, labeled)
		}
	})
}
