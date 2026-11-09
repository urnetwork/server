//go:build !sidecar_pending

package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A release leg until MG6's Gn-out (FS-54's untagged case): no share table
// exists, both routes answer that shares are not available, and nothing reaches
// the missing table.
func TestNetworkBridgeSharesAreUnavailableWithoutMg6(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if server.SidecarMg6Linked() {
			t.Fatal("a release build links a pending group")
		}
		var table *string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass('network_bridge_share')::text`).Scan(&table))
		})
		if table != nil {
			t.Fatal("network_bridge_share exists without MG6")
		}
		clientId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: server.NewId(), ClientId: &clientId})
		shareId := server.NewId()
		if _, err := GetNetworkBridgeShareForSidecar(shareId, clientSession); err == nil || !strings.HasPrefix(err.Error(), "503 ") {
			t.Fatalf("read: %v", err)
		}
		put := &PutNetworkBridgeShareArgs{Table: []byte{1}, Version: 1}
		if _, err := PutNetworkBridgeShareForSidecar(shareId, put, clientSession); err == nil || !strings.HasPrefix(err.Error(), "503 ") {
			t.Fatalf("write: %v", err)
		}
		if _, err := GetNetworkBridgeShare(ctx, shareId); !errors.Is(err, ErrNetworkBridgeShareUnavailable) {
			t.Fatalf("get: %v", err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			if err := CreateNetworkBridgeShareInTx(ctx, tx, shareId, server.NewId(), []byte{1}, 1); !errors.Is(err, ErrNetworkBridgeShareUnavailable) {
				t.Fatalf("create: %v", err)
			}
			if err := AcceptNetworkBridgeShareInTx(ctx, tx, shareId, server.NewId(), []byte{1}, 1, NetworkBridgeShareAcceptStatusActive); !errors.Is(err, ErrNetworkBridgeShareUnavailable) {
				t.Fatalf("accept: %v", err)
			}
		})
		if RemoveNetworkBridgeShare(ctx, shareId) {
			t.Fatal("a removal without MG6")
		}
	})
}
