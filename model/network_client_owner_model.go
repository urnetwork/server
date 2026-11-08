package model

import (
	"context"
	"crypto/ed25519"
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The P0 auth fix, §7.4's stage 1 (D-TEAM-1, I-6): no sidecar or agent
// credential administers the network or mints a login. A client whose tokens
// are in a sidecar's hands is marked in network_client_owner at mint, and a
// child of any client is refused alike. The router asks ClientRefusesAdmin on
// the 27 app admin and 10 own client payout routes (api/route_authz.go).
//
// The owner row belongs to migration group MG0, which is pending until its
// Gn-out. A binary that does not link MG0 reads only network_client, so it still
// sees every child, it marks nothing, and it refuses a request for a mark
// (server.SidecarMg0Linked).

// Settings of the client admin check.
type ClientAdminSettings struct {
	// The clients whose answer a process keeps, 32,768 to match clientAuthTimes'
	// bound. Each answer is immutable, so a full map clears and costs one more
	// read per client, never a wrong answer.
	CachedClientMaxCount int64
}

// The cache's bound.
func DefaultClientAdminSettings() *ClientAdminSettings {
	return &ClientAdminSettings{
		CachedClientMaxCount: 32 * 1024,
	}
}

// What this process has read. Each answer is immutable.
var clientRefusesAdminAnswers = newBoundedClientMap[bool](DefaultClientAdminSettings().CachedClientMaxCount)

var clientAdminLookupErrorCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "auth",
	Name:      "client_admin_lookup_errors_total",
	Help:      "Client admin checks with no answer: a failed read, or a client the token's network does not have. Each refuses, so urnetwork_auth_child_admin_calls_total counts it too",
})

func init() {
	prometheus.MustRegister(clientAdminLookupErrorCounter)
}

// Whether the client's tokens are refused on the admin routes that an ordinary
// network's client tokens still reach. True for a child (source_client_id is
// set), for an admin_refused owner row, and when the answer cannot be read. A
// top-level client with no owner row is a legacy client: the shipped apps call
// these routes with such a token until stage 3.
//
// Neither fact ever changes for a client: source_client_id and the owner row are
// written with the client. So an answer is cached for as long as this process
// can hold it, and only a failed lookup is asked again.
func ClientRefusesAdmin(ctx context.Context, networkId server.Id, clientId server.Id) bool {
	if refuses, ok := clientRefusesAdminAnswers.Load(clientId); ok {
		return refuses
	}
	sql := `
		SELECT network_client.source_client_id IS NOT NULL
		FROM network_client
		WHERE
			network_client.client_id = $1 AND
			network_client.network_id = $2
	`
	if server.SidecarMg0Linked() {
		sql = `
			SELECT
				network_client.source_client_id IS NOT NULL OR
				COALESCE(network_client_owner.admin_refused, false)
			FROM network_client
			LEFT JOIN network_client_owner ON network_client_owner.client_id = network_client.client_id
			WHERE
				network_client.client_id = $1 AND
				network_client.network_id = $2
		`
	}
	found := false
	refuses := false
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, sql, clientId, networkId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&refuses))
					found = true
				}
			})
		})
	}); r != nil || !found {
		clientAdminLookupErrorCounter.Inc()
		if glog.V(1) {
			glog.Infof("[auth][%s]client admin check has no answer for client %s\n", networkId, clientId)
		}
		return true
	}
	clientRefusesAdminAnswers.Store(clientId, refuses)
	return refuses
}

// Drops this process's cached answers, for a test that writes a client's rows
// directly after the client was asked about.
func Testing_ForgetClientRefusesAdmin() {
	clientRefusesAdminAnswers.Clear()
}

// A client's marks (§7.4). Both are set at mint and never change, and a marked
// client's children inherit both.
type clientMarks struct {
	// the client's tokens never administer the network
	adminRefused bool
	// the client's tokens are sidecar-held: 7-day expiry, the route allowlist and
	// the ClientKey proof (D-TEAM-14). Only sidecars send it; an app device minted
	// admin_refused, and so its window clients, carry none (owner decision 131).
	strictExp bool
}

// Whether there is anything to write in an owner row.
func (self clientMarks) marked() bool {
	return self.adminRefused || self.strictExp
}

// Both sets of marks: what a request asks for and what a parent passes down.
func (self clientMarks) union(other clientMarks) clientMarks {
	return clientMarks{
		adminRefused: self.adminRefused || other.adminRefused,
		strictExp:    self.strictExp || other.strictExp,
	}
}

// What a request asks for. A strict_exp client is always admin_refused, so a
// request for the one alone gets both.
func requestedClientMarks(authClient *AuthNetworkClientArgs) clientMarks {
	return clientMarks{
		adminRefused: authClient.AdminRefused || authClient.StrictExp,
		strictExp:    authClient.StrictExp,
	}
}

// The client's marks, none for a client of another network or with no owner row.
func readClientMarksInTx(ctx context.Context, tx server.PgTx, networkId server.Id, clientId server.Id) (marks clientMarks) {
	if !server.SidecarMg0Linked() {
		return
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT
				network_client_owner.admin_refused,
				network_client_owner.strict_exp
			FROM network_client_owner
			INNER JOIN network_client ON network_client.client_id = network_client_owner.client_id
			WHERE
				network_client_owner.client_id = $1 AND
				network_client.network_id = $2
		`,
		clientId,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&marks.adminRefused, &marks.strictExp))
		}
	})
	return
}

// Writes a new client's owner row. The caller checked the marks.
func insertClientOwnerInTx(ctx context.Context, tx server.PgTx, clientId server.Id, ownerUserId server.Id, marks clientMarks) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO network_client_owner (
				client_id,
				owner_user_id,
				admin_refused,
				strict_exp
			)
			VALUES ($1, $2, $3, $4)
		`,
		clientId,
		ownerUserId,
		marks.adminRefused,
		marks.strictExp,
	))
}

const (
	clientMarksUnavailableMessage = "This server cannot mark a client yet."
	clientMarksImmutableMessage   = "A client's marks are assigned at creation and cannot be changed."
	// D-TRUST-3: until stage 2 is live, a strict_exp client exists only in a
	// network sidecar.yml's preview_networks lists
	clientStrictExpNotPreviewMessage = "This network cannot hold sidecar clients yet."
	// a hosted proxy's host mints its token with no mark, so a strict_exp client
	// never has one
	clientStrictExpProxyMessage = "A sidecar client cannot host a proxy."
)

// Why a mint of a client with these marks is refused, or "". Every mint that ends
// in a strict_exp token asks, an inherited mark included.
func refuseClientMarks(networkId server.Id, marks clientMarks, proxy bool) string {
	if !marks.strictExp {
		return ""
	}
	if !server.CurrentSidecarConfig().PreviewNetwork(networkId) {
		return clientStrictExpNotPreviewMessage
	}
	if proxy {
		return clientStrictExpProxyMessage
	}
	return ""
}

// The client has no key a proof could be checked against.
var ErrClientProofKeyNotBound = errors.New("Client has no bound key.")

// The key that proves a strict_exp bearer's ClientKey (§7.4): the client's newest
// sidecar_identity_binding key, else enroll's client_public_key.
//
// The binding's key is GetClientBindingKey's, which refuses an older
// generation's. Enroll's network_client_owner.client_public_key is
// plan-TEAM-enroll-client-key's MG1 column, which POST /network/enroll
// (TEAM-M1-04) writes and reads here when it lands; until then a client with no
// binding has no key, and its proof is refused, as §7.4 rules for a client with
// none bound.
func ClientProofPublicKey(ctx context.Context, clientId server.Id) (ed25519.PublicKey, error) {
	clientPublicKey, err := GetClientBindingKey(ctx, clientId)
	if err != nil || len(clientPublicKey) != ed25519.PublicKeySize {
		return nil, ErrClientProofKeyNotBound
	}
	return ed25519.PublicKey(clientPublicKey), nil
}
