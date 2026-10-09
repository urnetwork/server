package model

// A client token acts only for its own client and the clients it created
// (AUTHZ1.md): it cannot mint a top-level client, create a child of another
// client, reissue another client's token, remove another client, or rename
// another client's device. Each refusal writes nothing, and the network
// credential beside it still can.

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// clientTokenTestSession is the session of the client's own token.
func clientTokenTestSession(ctx context.Context, userSession *session.ClientSession, clientId server.Id, deviceId server.Id) *session.ClientSession {
	return session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: userSession.ByJwt.NetworkId,
		UserId:    userSession.ByJwt.UserId,
		DeviceId:  &deviceId,
		ClientId:  &clientId,
	})
}

func clientTokenTestActive(ctx context.Context, clientId server.Id) (active bool) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT active FROM network_client WHERE client_id = $1`,
			clientId,
		).Scan(&active))
	})
	return
}

func clientTokenTestDeviceName(ctx context.Context, deviceId server.Id) (deviceName string) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT device_name FROM device WHERE device_id = $1`,
			deviceId,
		).Scan(&deviceName))
	})
	return
}

func TestAuthNetworkClientClientTokenCreatesOnlyItsOwnChildren(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		clientId, deviceId := authClientTestClient(ctx, t, userSession)
		otherClientId, _ := authClientTestClient(ctx, t, userSession)
		clientSession := clientTokenTestSession(ctx, userSession, clientId, deviceId)

		for _, c := range []struct {
			name           string
			sourceClientId *server.Id
			message        string
		}{
			{name: "a top-level client", sourceClientId: nil, message: "A client token cannot create a top-level client."},
			{name: "a child of another client", sourceClientId: &otherClientId, message: "Client does not exist."},
		} {
			clientCountBefore, deviceCountBefore, _ := authClientTestNetworkCounts(ctx, networkId)
			result, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					SourceClientId: c.sourceClientId,
					Description:    "attacker",
					DeviceSpec:     "attacker",
				},
				clientSession,
			)
			connect.AssertEqual(t, err, nil)
			if result == nil || result.Error == nil || result.Error.Message != c.message {
				t.Fatalf("%s: answered %+v, want the refusal %q", c.name, result, c.message)
			}
			if result.ClientId != nil || result.ByClientJwt != nil {
				t.Fatalf("%s: the refusal returned credentials", c.name)
			}
			clientCount, deviceCount, _ := authClientTestNetworkCounts(ctx, networkId)
			if clientCount != clientCountBefore || deviceCount != deviceCountBefore {
				t.Fatalf("%s: created %d clients and %d devices", c.name, clientCount-clientCountBefore, deviceCount-deviceCountBefore)
			}
		}

		// a child of its own client, on its own device
		result, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				SourceClientId: &clientId,
				Description:    "window",
				DeviceSpec:     "window",
			},
			clientSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.ClientId, nil)
		var childDeviceId server.Id
		var childSourceClientId *server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT device_id, source_client_id FROM network_client WHERE client_id = $1`,
				*result.ClientId,
			).Scan(&childDeviceId, &childSourceClientId))
		})
		connect.AssertEqual(t, childDeviceId, deviceId)
		connect.AssertNotEqual(t, childSourceClientId, nil)
		connect.AssertEqual(t, *childSourceClientId, clientId)

		// the network credential still mints a top-level client
		result, err = AuthNetworkClient(&AuthNetworkClientArgs{Description: "device", DeviceSpec: "device"}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.ClientId, nil)
	})
}

func TestAuthNetworkClientClientTokenReissuesOnlyItsOwnClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		clientId, deviceId := authClientTestClient(ctx, t, userSession)
		otherClientId, _ := authClientTestClient(ctx, t, userSession)
		clientSession := clientTokenTestSession(ctx, userSession, clientId, deviceId)

		childResult, err := AuthNetworkClient(&AuthNetworkClientArgs{SourceClientId: &clientId, Description: "window", DeviceSpec: "window"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, childResult.Error, nil)
		childClientId := *childResult.ClientId

		// another client's token is refused as a client that does not exist,
		// and its client is not touched
		descriptionBefore, authTimeBefore, _ := authClientTestClientState(ctx, otherClientId)
		result, err := AuthNetworkClient(&AuthNetworkClientArgs{ClientId: &otherClientId, Description: "attacker", DeviceSpec: "attacker"}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result == nil || result.Error == nil || result.Error.Message != "Client does not exist." || result.ByClientJwt != nil {
			t.Fatalf("reissue of another client answered %+v, want the refusal", result)
		}
		description, authTime, _ := authClientTestClientState(ctx, otherClientId)
		connect.AssertEqual(t, description, descriptionBefore)
		connect.AssertEqual(t, authTime, authTimeBefore)

		// its own token and its child's are reissued for exactly those clients
		for _, ownClientId := range []server.Id{clientId, childClientId} {
			result, err := AuthNetworkClient(&AuthNetworkClientArgs{ClientId: &ownClientId, Description: "after", DeviceSpec: "after"}, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, nil)
			connect.AssertNotEqual(t, result.ByClientJwt, nil)
			reissued, err := jwt.ParseByJwtUnverified(ctx, *result.ByClientJwt)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, reissued.ClientId, nil)
			connect.AssertEqual(t, *reissued.ClientId, ownClientId)
		}

		// the network credential still reissues any client
		result, err = AuthNetworkClient(&AuthNetworkClientArgs{ClientId: &otherClientId, Description: "device", DeviceSpec: "device"}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.ByClientJwt, nil)
	})
}

func TestRemoveNetworkClientClientTokenRemovesOnlyItsOwnClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		clientId, deviceId := authClientTestClient(ctx, t, userSession)
		otherClientId, _ := authClientTestClient(ctx, t, userSession)
		clientSession := clientTokenTestSession(ctx, userSession, clientId, deviceId)

		childResult, err := AuthNetworkClient(&AuthNetworkClientArgs{SourceClientId: &clientId, Description: "window", DeviceSpec: "window"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, childResult.Error, nil)
		childClientId := *childResult.ClientId

		// another client, or a client of nothing, is refused as one that does
		// not exist and stays active
		for _, targetClientId := range []server.Id{otherClientId, server.NewId()} {
			result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: targetClientId}, clientSession)
			connect.AssertEqual(t, err, nil)
			if result == nil || result.Error == nil || result.Error.Message != "Client does not exist." {
				t.Fatalf("removing %s answered %+v, want the refusal", targetClientId, result)
			}
		}
		connect.AssertEqual(t, clientTokenTestActive(ctx, otherClientId), true)

		// its child, then itself
		for _, ownClientId := range []server.Id{childClientId, clientId} {
			result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: ownClientId}, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, nil)
			connect.AssertEqual(t, clientTokenTestActive(ctx, ownClientId), false)
		}

		// the network credential still removes any client
		result, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: otherClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, clientTokenTestActive(ctx, otherClientId), false)
	})
}

func TestDeviceSetNameClientTokenRenamesOnlyItsOwnDevice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		clientId, deviceId := authClientTestClient(ctx, t, userSession)
		_, otherDeviceId := authClientTestClient(ctx, t, userSession)
		clientSession := clientTokenTestSession(ctx, userSession, clientId, deviceId)

		otherNameBefore := clientTokenTestDeviceName(ctx, otherDeviceId)
		result, err := DeviceSetName(&DeviceSetNameArgs{DeviceId: otherDeviceId, DeviceName: "attacker"}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result == nil || result.Error == nil || result.Error.Message != "Device does not exist." {
			t.Fatalf("renaming another device answered %+v, want the refusal", result)
		}
		connect.AssertEqual(t, clientTokenTestDeviceName(ctx, otherDeviceId), otherNameBefore)

		// its own device, also from its child
		result, err = DeviceSetName(&DeviceSetNameArgs{DeviceId: deviceId, DeviceName: "mine"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, clientTokenTestDeviceName(ctx, deviceId), "mine")
		childResult, err := AuthNetworkClient(&AuthNetworkClientArgs{SourceClientId: &clientId, Description: "window", DeviceSpec: "window"}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, childResult.Error, nil)
		childSession := clientTokenTestSession(ctx, userSession, *childResult.ClientId, deviceId)
		result, err = DeviceSetName(&DeviceSetNameArgs{DeviceId: deviceId, DeviceName: "mine again"}, childSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, clientTokenTestDeviceName(ctx, deviceId), "mine again")

		// the network credential still renames any device
		result, err = DeviceSetName(&DeviceSetNameArgs{DeviceId: otherDeviceId, DeviceName: "renamed"}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, clientTokenTestDeviceName(ctx, otherDeviceId), "renamed")
	})
}

// The Embed plan is what makes a network refuse its client tokens on the app
// admin routes.
func TestNetworkRefusesClientAdminFollowsTheEmbedPlan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, _ := authClientTestNetwork(ctx, "test")
		connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, networkId), false)

		connect.AssertEqual(t, SetNetworkTopLevelClientLimit(ctx, networkId, 1000), nil)
		connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, networkId), true)

		ClearNetworkTopLevelClientLimit(ctx, networkId)
		connect.AssertEqual(t, NetworkRefusesClientAdmin(ctx, networkId), false)
	})
}
