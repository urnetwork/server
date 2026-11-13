package model

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

// §21.6's list on a private database (TLS-M5-13): it pages every network's
// grants by cursor with none given twice, drops what has expired, and answers
// unavailable, never empty, where it cannot read the grants.

// A network with a log, as api's append path leaves its head.
func tlsPublishedNamesTestNetwork(ctx context.Context) server.Id {
	networkId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_acl_head (network_id, version, chain) VALUES ($1, 1, $2)`,
			networkId,
			make([]byte, 32),
		))
	})
	return networkId
}

func TestTlsPublishedNamesUnavailableWithoutASource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		SetTlsPublishedNamesSource(nil)
		if names, cursor, err := ListTlsPublishedNames(ctx, nil, server.NowUtc()); !errors.Is(err, ErrTlsPublishedNamesUnavailable) || names != nil || cursor != nil {
			t.Fatalf("a list with no source answered %v, %v, %v", names, cursor, err)
		}
		if server.SidecarMg2Linked() {
			return
		}
		SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, now time.Time) ([]*TlsPublishedName, error) {
			return nil, nil
		})
		defer SetTlsPublishedNamesSource(nil)
		if _, _, err := ListTlsPublishedNames(ctx, nil, server.NowUtc()); !errors.Is(err, ErrTlsPublishedNamesUnavailable) {
			t.Fatalf("a list without MG2 answered %v", err)
		}
	})
}

func TestTlsPublishedNamesPageByCursorAndDropExpired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg2Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		networkIds := []server.Id{}
		for range 5 {
			networkIds = append(networkIds, tlsPublishedNamesTestNetwork(ctx))
		}
		slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
			return bytes.Compare(a[:], b[:])
		})
		// the first network publishes three names and an expired one, the third none, the rest one each
		networkIdNames := map[server.Id][]*TlsPublishedName{
			networkIds[0]: {
				{Host: "shop", Port: 443, Label: "acme", Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC},
				{Host: "blog", Port: 8443, Label: "acme", Audience: protocol.AclAudience_ACL_AUDIENCE_MEMBERS, ExpireTime: now.Add(time.Hour)},
				nil,
				{Host: "blog", Port: 443, Alias: "blog.example.test", Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC},
				{Host: "old", Port: 443, Label: "acme", Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC, ExpireTime: now.Add(-time.Second)},
				{Host: "ending", Port: 443, Label: "acme", Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC, ExpireTime: now},
			},
			networkIds[1]: {{Host: "a", Port: 443, Audience: protocol.AclAudience_ACL_AUDIENCE_LINK}},
			networkIds[3]: {{Host: "b", Port: 443, NetworkId: networkIds[0]}},
			networkIds[4]: {{Host: "c", Port: 443}},
		}
		sourceCalls := 0
		SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, sourceNow time.Time) ([]*TlsPublishedName, error) {
			sourceCalls += 1
			if !sourceNow.Equal(now) {
				t.Errorf("the source was asked at %v", sourceNow)
			}
			return networkIdNames[networkId], nil
		})
		defer SetTlsPublishedNamesSource(nil)

		type listed struct {
			networkId server.Id
			host      string
			port      int
		}
		want := []listed{
			{networkIds[0], "blog", 443},
			{networkIds[0], "blog", 8443},
			{networkIds[0], "shop", 443},
			{networkIds[1], "a", 443},
			{networkIds[3], "b", 443},
			{networkIds[4], "c", 443},
		}
		// other tests' networks may hold heads too: the source answers nothing for them
		for _, settings := range []*TlsPublishedNamesSettings{
			{Limit: 1000, NetworkBatchCount: 200},
			{Limit: 2, NetworkBatchCount: 2},
			{Limit: 1, NetworkBatchCount: 1},
			{Limit: 3, NetworkBatchCount: 1},
		} {
			got := []listed{}
			var cursor *TlsPublishedNamesCursor
			pageCount := 0
			for {
				names, next, err := ListTlsPublishedNamesWithSettings(ctx, cursor, now, settings)
				if err != nil {
					t.Fatal(err)
				}
				if settings.Limit < len(names) {
					t.Fatalf("a page of %d past its limit %d", len(names), settings.Limit)
				}
				for _, name := range names {
					got = append(got, listed{name.NetworkId, name.Host, name.Port})
				}
				pageCount += 1
				if next == nil {
					break
				}
				if len(names) == 0 || 100 < pageCount {
					t.Fatalf("a page of %d names gave a cursor, page %d", len(names), pageCount)
				}
				cursor = next
			}
			if !slices.Equal(got, want) {
				t.Fatalf("limit %d listed %v, want %v", settings.Limit, got, want)
			}
		}
		if sourceCalls == 0 {
			t.Fatal("the source was never asked")
		}
		// the source's own value is not changed by the list
		if networkIdNames[networkIds[3]][0].NetworkId != networkIds[0] {
			t.Fatal("the list wrote into the source's name")
		}

		// a network the source cannot answer for fails the page
		sourceErr := errors.New("no verified state")
		SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, now time.Time) ([]*TlsPublishedName, error) {
			if networkId == networkIds[1] {
				return nil, sourceErr
			}
			return networkIdNames[networkId], nil
		})
		if names, cursor, err := ListTlsPublishedNames(ctx, nil, now); !errors.Is(err, sourceErr) || names != nil || cursor != nil {
			t.Fatalf("a failing network answered %v, %v, %v", names, cursor, err)
		}
	})
}
