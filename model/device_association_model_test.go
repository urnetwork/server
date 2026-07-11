package model

// basic test for each function

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestDeviceAdopt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()

		deviceIdA := server.NewId()
		clientIdA := server.NewId()
		guestMode := false
		isPro := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode, isPro),
		)

		clientSessionNoAuth := session.Testing_CreateClientSession(
			ctx,
			nil,
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		clientsResult0, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result1.Error, nil)
		assert.NotEqual(t, result1.AdoptCode, "")
		assert.NotEqual(t, result1.AdoptSecret, "")

		qrResult0, err := DeviceAdoptCodeQR(
			&DeviceAdoptCodeQRArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, qrResult0.PngBytes, nil)

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult0.PendingAdoptionDevices), 1)
		assert.Equal(t, len(associationResult0.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult0.OutgoingSharedDevices), 0)

		result4, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             result1.AdoptCode,
				AdoptSecret:           result1.AdoptSecret,
				AssociatedNetworkName: result3.AssociatedNetworkName,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result4.Error, nil)
		// assert.Equal(t, result4.AssociatedNetworkName, "a")
		assert.NotEqual(t, result4.ByClientJwt, "")
		byJwt, err := jwt.ParseByJwt(ctx, result4.ByClientJwt)
		assert.Equal(t, err, nil)
		assert.Equal(t, byJwt.NetworkId, networkIdA)
		assert.Equal(t, byJwt.NetworkName, "a")
		assert.Equal(t, byJwt.UserId, userIdA)
		assert.Equal(t, byJwt.GuestMode, false)

		// at this point there should be no adopt association
		associationResult1, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult1.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult1.OutgoingSharedDevices), 0)

		clientResult1, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientResult1.Clients), 2)
	})
}

// TestDeviceConfirmAdoptWrongSecretRejected reproduces the VDP report in
// vdp/VDP1.md. POST /device/confirm-adopt accepts an adopt_secret, but
// DeviceConfirmAdopt never validates it: the device_adopt UPDATE and the JWT
// issuance are gated only on the adopt_code and the adopting network's name.
// The network name is itself leaked by the unauthenticated /device/adopt-status,
// and all three endpoints (create-adopt-code, adopt-status, confirm-adopt) are
// WrapWithInputNoAuth. So anyone who observes a pending adopt code — which the
// product shares by design via QR code / the https://app.bringyour.com/?add=CODE
// deep link — can mint a valid client JWT in the adopting network without the
// secret. Contrast DeviceRemoveAdoptCode in the same file, which correctly gates
// on `adopt_secret = $2`.
//
// The happy path (correct secret succeeds) is covered by TestDeviceAdopt. This
// test asserts the SECURE expectation — a wrong secret must NOT mint a client
// JWT — so it FAILS against the current (vulnerable) code and will pass once a
// constant-time `adopt_secret` check is added to the device_adopt UPDATE.
func TestDeviceConfirmAdoptWrongSecretRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// The victim network that adopts the device.
		networkIdA := server.NewId()
		userIdA := server.NewId()
		deviceIdA := server.NewId()
		clientIdA := server.NewId()

		victimSession := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", false, false),
		)
		// create-adopt-code, adopt-status and confirm-adopt need no credentials.
		noAuthSession := session.Testing_CreateClientSession(ctx, nil)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		// Step 2: the device creates an adopt code. adopt_secret is meant to stay
		// private to the device; adopt_code is shared publicly (QR / deep link).
		created, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{DeviceName: "devicec", DeviceSpec: "specc"},
			noAuthSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, created.Error, nil)
		assert.NotEqual(t, created.AdoptCode, "")
		assert.NotEqual(t, created.AdoptSecret, "")

		// Step 3: the victim adopts the (public) code.
		added, err := DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, victimSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, added.Error, nil)

		// Step 4: unauthenticated adopt-status leaks the victim's network name.
		status, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{AdoptCode: created.AdoptCode},
			noAuthSession,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, status.Error, nil)
		assert.Equal(t, status.AssociatedNetworkName, "a")

		// Step 5: confirm with the leaked network name but a DELIBERATELY WRONG
		// secret. A legitimate device proves possession of adopt_secret here; an
		// attacker who only eavesdropped the code does not have it.
		wrongSecret := "00000000000000000000000000000000deadbeefdeadbeef"
		assert.NotEqual(t, wrongSecret, created.AdoptSecret)

		confirmed, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             created.AdoptCode,
				AdoptSecret:           wrongSecret,
				AssociatedNetworkName: status.AssociatedNetworkName,
			},
			noAuthSession,
		)
		assert.Equal(t, err, nil)

		// SECURE expectation: a wrong secret must not yield a client credential.
		if confirmed != nil && confirmed.ByClientJwt != "" {
			byJwt, parseErr := jwt.ParseByJwt(ctx, confirmed.ByClientJwt)
			if parseErr == nil {
				t.Fatalf("SECURITY (VDP1): /device/confirm-adopt minted a client JWT with a WRONG "+
					"adopt_secret — authentication bypass. Minted credential networkId=%s networkName=%q "+
					"userId=%s guestMode=%v. DeviceConfirmAdopt never references confirmAdopt.AdoptSecret; "+
					"add `AND adopt_secret = $N` (constant-time) to the device_adopt UPDATE, as "+
					"DeviceRemoveAdoptCode already does.",
					byJwt.NetworkId, byJwt.NetworkName, byJwt.UserId, byJwt.GuestMode)
			}
			t.Fatalf("SECURITY (VDP1): /device/confirm-adopt returned a by_client_jwt with a WRONG " +
				"adopt_secret — authentication bypass.")
		}

		// After the fix, confirm affects 0 rows and returns the generic error.
		assert.NotEqual(t, confirmed.Error, nil)
	})
}

func TestDeviceAdoptPartialOfferRemove(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// add adopt new code
		// remove association from the added account should remove the adopt code

		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()

		deviceIdA := server.NewId()
		clientIdA := server.NewId()
		guestMode := false
		isPro := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode, isPro),
		)

		clientSessionNoAuth := session.Testing_CreateClientSession(
			ctx,
			nil,
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		clientsResult0, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result1.Error, nil)
		assert.NotEqual(t, result1.AdoptCode, "")
		assert.NotEqual(t, result1.AdoptSecret, "")

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult0.PendingAdoptionDevices), 1)
		assert.Equal(t, len(associationResult0.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult0.OutgoingSharedDevices), 0)

		removeResult0, err := DeviceRemoveAdoptCode(
			&DeviceRemoveAdoptCodeArgs{
				AdoptCode:   result1.AdoptCode,
				AdoptSecret: result1.AdoptSecret,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, removeResult0.Error, nil)

		associationResult1, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult1.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult1.OutgoingSharedDevices), 0)

	})
}

func TestDeviceAdoptPartialOwnerRemove(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		// remove the adopt code before confirm
		// TODO test remove adopt code after confirm should have no impact

		ctx := context.Background()

		networkIdA := server.NewId()

		userIdA := server.NewId()

		deviceIdA := server.NewId()
		clientIdA := server.NewId()
		guestMode := false
		isPro := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode, isPro),
		)

		clientSessionNoAuth := session.Testing_CreateClientSession(
			ctx,
			nil,
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		clientsResult0, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result1.Error, nil)
		assert.NotEqual(t, result1.AdoptCode, "")
		assert.NotEqual(t, result1.AdoptSecret, "")

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult0.PendingAdoptionDevices), 1)
		assert.Equal(t, len(associationResult0.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult0.OutgoingSharedDevices), 0)

		removeResult0, err := DeviceRemoveAssociation(
			&DeviceRemoveAssociationArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, removeResult0.Error, nil)

		associationResult1, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult1.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult1.OutgoingSharedDevices), 0)

	})
}

func TestDeviceShare(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkIdA := server.NewId()
		networkIdB := server.NewId()

		userIdA := server.NewId()
		userIdB := server.NewId()

		deviceIdA := server.NewId()
		clientIdA := server.NewId()

		deviceIdB := server.NewId()
		clientIdB := server.NewId()
		guestMode := false
		isPro := false

		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", guestMode, isPro),
		)

		clientSessionB := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdB, userIdB, "b", guestMode, isPro),
		)

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		Testing_CreateDevice(ctx, networkIdB, deviceIdB, clientIdB, "deviceb", "specb")

		clientsResult0, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult0.Clients), 1)

		clientsResult1, err := GetNetworkClients(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult1.Clients), 1)

		result1, err := DeviceCreateShareCode(
			&DeviceCreateShareCodeArgs{
				ClientId:   clientIdA,
				DeviceName: "devicea",
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result1.Error, nil)
		assert.NotEqual(t, result1.ShareCode, "")

		associationResult0, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult0.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult0.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult0.OutgoingSharedDevices), 1)
		assert.Equal(t, associationResult0.OutgoingSharedDevices[0].Pending, true)
		assert.Equal(t, associationResult0.OutgoingSharedDevices[0].NetworkName, "")
		assert.Equal(t, associationResult0.OutgoingSharedDevices[0].DeviceName, "devicea")

		shareStatus1, err := DeviceShareStatus(
			&DeviceShareStatusArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, shareStatus1.Error, nil)

		qrResult0, err := DeviceShareCodeQR(
			&DeviceShareCodeQRArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, qrResult0.PngBytes, nil)

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.ShareCode,
			},
			clientSessionB,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result2.Error, nil)

		result3, err := DeviceShareStatus(
			&DeviceShareStatusArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result3.Error, nil)

		// at this point there should be share association pending

		associationResult1, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult1.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult1.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult1.OutgoingSharedDevices), 1)
		assert.Equal(t, associationResult1.OutgoingSharedDevices[0].Pending, true)
		assert.Equal(t, associationResult1.OutgoingSharedDevices[0].NetworkName, "b")
		assert.Equal(t, associationResult1.OutgoingSharedDevices[0].DeviceName, "devicea")

		associationResult2, err := DeviceAssociations(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult2.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult2.IncomingSharedDevices), 1)
		assert.Equal(t, len(associationResult2.OutgoingSharedDevices), 0)
		assert.Equal(t, associationResult2.IncomingSharedDevices[0].Pending, true)
		assert.Equal(t, associationResult2.IncomingSharedDevices[0].NetworkName, "a")
		assert.Equal(t, associationResult2.IncomingSharedDevices[0].DeviceName, "devicea")

		result4, err := DeviceConfirmShare(
			&DeviceConfirmShareArgs{
				ShareCode:             result1.ShareCode,
				AssociatedNetworkName: result3.AssociatedNetworkName,
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, result4.Error, nil)
		assert.Equal(t, result4.AssociatedNetworkName, "b")

		// at this point there should be a share association not pending, as outgoing for A, incoming for B
		associationResult3, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult3.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult3.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult3.OutgoingSharedDevices), 1)
		assert.Equal(t, associationResult3.OutgoingSharedDevices[0].Pending, false)
		assert.Equal(t, associationResult3.OutgoingSharedDevices[0].NetworkName, "b")
		assert.Equal(t, associationResult3.OutgoingSharedDevices[0].DeviceName, "devicea")

		associationResult4, err := DeviceAssociations(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult4.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult4.IncomingSharedDevices), 1)
		assert.Equal(t, len(associationResult4.OutgoingSharedDevices), 0)
		assert.Equal(t, associationResult4.IncomingSharedDevices[0].Pending, false)
		assert.Equal(t, associationResult4.IncomingSharedDevices[0].NetworkName, "a")
		assert.Equal(t, associationResult4.IncomingSharedDevices[0].DeviceName, "devicea")

		setNameResult1, err := DeviceSetAssociationName(
			&DeviceSetAssociationNameArgs{
				Code:       result1.ShareCode,
				DeviceName: "That device I shared",
			},
			clientSessionA,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, setNameResult1.Error, nil)

		setNameResult2, err := DeviceSetAssociationName(
			&DeviceSetAssociationNameArgs{
				Code:       result1.ShareCode,
				DeviceName: "My new device from friend",
			},
			clientSessionB,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, setNameResult2.Error, nil)

		associationResult5, err := DeviceAssociations(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult5.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult5.IncomingSharedDevices), 0)
		assert.Equal(t, len(associationResult5.OutgoingSharedDevices), 1)
		assert.Equal(t, associationResult5.OutgoingSharedDevices[0].Pending, false)
		assert.Equal(t, associationResult5.OutgoingSharedDevices[0].NetworkName, "b")
		assert.Equal(t, associationResult5.OutgoingSharedDevices[0].DeviceName, "That device I shared")

		associationResult6, err := DeviceAssociations(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(associationResult6.PendingAdoptionDevices), 0)
		assert.Equal(t, len(associationResult6.IncomingSharedDevices), 1)
		assert.Equal(t, len(associationResult6.OutgoingSharedDevices), 0)
		assert.Equal(t, associationResult6.IncomingSharedDevices[0].Pending, false)
		assert.Equal(t, associationResult6.IncomingSharedDevices[0].NetworkName, "a")
		assert.Equal(t, associationResult6.IncomingSharedDevices[0].DeviceName, "My new device from friend")

		clientsResult2, err := GetNetworkClients(clientSessionA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult2.Clients), 1)

		clientsResult3, err := GetNetworkClients(clientSessionB)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsResult3.Clients), 1)
	})
}

// --- Security regression tests for the device-association surface ---
//
// These lock down the authn/authz invariants adjacent to the adopt_secret bypass
// fixed in DeviceConfirmAdopt (see TestDeviceConfirmAdoptWrongSecretRejected and
// vdp/VDP1.md): create / add / confirm / remove / rename must each reject callers
// that don't hold the right secret, don't own the resource, or aren't a party to
// the association.

func testingAdoptNetwork(ctx context.Context, name string) (clientSession *session.ClientSession, networkId server.Id, userId server.Id) {
	networkId = server.NewId()
	userId = server.NewId()
	clientSession = session.Testing_CreateClientSession(
		ctx,
		jwt.NewByJwt(networkId, userId, name, false, false),
	)
	Testing_CreateNetwork(ctx, networkId, name, userId)
	return
}

// A confirm with the correct secret but the wrong network name must not mint a JWT.
func TestDeviceConfirmAdoptWrongNetworkNameRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		victim, _, _ := testingAdoptNetwork(ctx, "a")
		noAuth := session.Testing_CreateClientSession(ctx, nil)

		created, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{DeviceName: "d", DeviceSpec: "s"}, noAuth)
		assert.Equal(t, err, nil)

		added, err := DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, victim)
		assert.Equal(t, err, nil)
		assert.Equal(t, added.Error, nil)

		confirmed, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             created.AdoptCode,
				AdoptSecret:           created.AdoptSecret,
				AssociatedNetworkName: "some-other-network",
			}, noAuth)
		assert.Equal(t, err, nil)
		assert.Equal(t, confirmed.ByClientJwt, "")
		assert.NotEqual(t, confirmed.Error, nil)
	})
}

// A confirm before anyone has adopted the code (owner_network_id IS NULL) must not mint a JWT.
func TestDeviceConfirmAdoptBeforeAdoptRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testingAdoptNetwork(ctx, "a") // "a" exists but never adopts this code
		noAuth := session.Testing_CreateClientSession(ctx, nil)

		created, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{DeviceName: "d", DeviceSpec: "s"}, noAuth)
		assert.Equal(t, err, nil)

		confirmed, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             created.AdoptCode,
				AdoptSecret:           created.AdoptSecret,
				AssociatedNetworkName: "a",
			}, noAuth)
		assert.Equal(t, err, nil)
		assert.Equal(t, confirmed.ByClientJwt, "")
		assert.NotEqual(t, confirmed.Error, nil)
	})
}

// A second confirm of an already-confirmed adopt must not mint another credential.
func TestDeviceConfirmAdoptReplayRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		victim, _, _ := testingAdoptNetwork(ctx, "a")
		noAuth := session.Testing_CreateClientSession(ctx, nil)

		created, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{DeviceName: "d", DeviceSpec: "s"}, noAuth)
		assert.Equal(t, err, nil)

		_, err = DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, victim)
		assert.Equal(t, err, nil)

		args := &DeviceConfirmAdoptArgs{
			AdoptCode:             created.AdoptCode,
			AdoptSecret:           created.AdoptSecret,
			AssociatedNetworkName: "a",
		}

		first, err := DeviceConfirmAdopt(args, noAuth)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, first.ByClientJwt, "")

		clientsAfterFirst, err := GetNetworkClients(victim)
		assert.Equal(t, err, nil)

		second, err := DeviceConfirmAdopt(args, noAuth)
		assert.Equal(t, err, nil)
		assert.Equal(t, second.ByClientJwt, "")
		assert.NotEqual(t, second.Error, nil)

		clientsAfterSecond, err := GetNetworkClients(victim)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(clientsAfterSecond.Clients), len(clientsAfterFirst.Clients))
	})
}

// Once network A has adopted a code, a different network B cannot take it over.
func TestDeviceAdoptCannotHijackAfterAdopt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		netA, networkIdA, _ := testingAdoptNetwork(ctx, "a")
		netB, _, _ := testingAdoptNetwork(ctx, "b")
		noAuth := session.Testing_CreateClientSession(ctx, nil)

		created, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{DeviceName: "d", DeviceSpec: "s"}, noAuth)
		assert.Equal(t, err, nil)

		addedA, err := DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netA)
		assert.Equal(t, err, nil)
		assert.Equal(t, addedA.Error, nil)

		// B tries to adopt the same, already-owned code.
		addedB, err := DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netB)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, addedB.Error, nil)

		// The device confirms for A (the true owner); the JWT is bound to A, not B.
		confirmed, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             created.AdoptCode,
				AdoptSecret:           created.AdoptSecret,
				AssociatedNetworkName: "a",
			}, noAuth)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, confirmed.ByClientJwt, "")
		byJwt, err := jwt.ParseByJwt(ctx, confirmed.ByClientJwt)
		assert.Equal(t, err, nil)
		assert.Equal(t, byJwt.NetworkId, networkIdA)
	})
}

// remove-adopt-code must reject a wrong secret (the sibling gate the confirm fix mirrors).
func TestDeviceRemoveAdoptCodeWrongSecretRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		netA, _, _ := testingAdoptNetwork(ctx, "a")
		noAuth := session.Testing_CreateClientSession(ctx, nil)

		created, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{DeviceName: "d", DeviceSpec: "s"}, noAuth)
		assert.Equal(t, err, nil)

		_, err = DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netA)
		assert.Equal(t, err, nil)

		// Wrong secret: must not remove.
		wrong, err := DeviceRemoveAdoptCode(
			&DeviceRemoveAdoptCodeArgs{AdoptCode: created.AdoptCode, AdoptSecret: "wrong-secret"}, noAuth)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, wrong.Error, nil)

		// Still present: A still sees a pending adoption.
		assoc, err := DeviceAssociations(netA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(assoc.PendingAdoptionDevices), 1)

		// Correct secret: removes.
		ok, err := DeviceRemoveAdoptCode(
			&DeviceRemoveAdoptCodeArgs{AdoptCode: created.AdoptCode, AdoptSecret: created.AdoptSecret}, noAuth)
		assert.Equal(t, err, nil)
		assert.Equal(t, ok.Error, nil)
	})
}

// A network cannot mint a share code for a client it does not own.
func TestDeviceCreateShareCodeForeignClientRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		netA, _, _ := testingAdoptNetwork(ctx, "a")
		_, networkIdB, _ := testingAdoptNetwork(ctx, "b")

		deviceIdB := server.NewId()
		clientIdB := server.NewId()
		Testing_CreateDevice(ctx, networkIdB, deviceIdB, clientIdB, "deviceb", "specb")

		// A tries to share B's client.
		result, err := DeviceCreateShareCode(
			&DeviceCreateShareCodeArgs{ClientId: clientIdB, DeviceName: "stolen"}, netA)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.Error, nil)
		assert.Equal(t, result.ShareCode, "")
	})
}

// A network cannot add (accept) its own share code.
func TestDeviceAddOwnShareCodeRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		netA, networkIdA, _ := testingAdoptNetwork(ctx, "a")

		deviceIdA := server.NewId()
		clientIdA := server.NewId()
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		share, err := DeviceCreateShareCode(
			&DeviceCreateShareCodeArgs{ClientId: clientIdA, DeviceName: "devicea"}, netA)
		assert.Equal(t, err, nil)
		assert.Equal(t, share.Error, nil)

		added, err := DeviceAdd(&DeviceAddArgs{Code: share.ShareCode}, netA)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, added.Error, nil)
	})
}

// A network that is neither the share's source nor its guest must not be able to
// confirm it. DeviceConfirmShare currently never references clientSession.ByJwt,
// so this asserts the secure expectation and is expected to fail until a caller
// check is added.
func TestDeviceConfirmShareByUnrelatedNetworkRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		netA, networkIdA, _ := testingAdoptNetwork(ctx, "a") // source
		netB, _, _ := testingAdoptNetwork(ctx, "b")          // guest
		netC, _, _ := testingAdoptNetwork(ctx, "c")          // unrelated

		deviceIdA := server.NewId()
		clientIdA := server.NewId()
		Testing_CreateDevice(ctx, networkIdA, deviceIdA, clientIdA, "devicea", "speca")

		share, err := DeviceCreateShareCode(
			&DeviceCreateShareCodeArgs{ClientId: clientIdA, DeviceName: "devicea"}, netA)
		assert.Equal(t, err, nil)
		assert.Equal(t, share.Error, nil)

		added, err := DeviceAdd(&DeviceAddArgs{Code: share.ShareCode}, netB)
		assert.Equal(t, err, nil)
		assert.Equal(t, added.Error, nil)

		// C, knowing only the share code and the guest's network name, confirms.
		confirmed, err := DeviceConfirmShare(
			&DeviceConfirmShareArgs{ShareCode: share.ShareCode, AssociatedNetworkName: "b"}, netC)
		assert.Equal(t, err, nil)
		if confirmed != nil && confirmed.Error == nil {
			t.Fatalf("SECURITY (adjacent): unrelated network C (neither source nor guest) confirmed a " +
				"device share via DeviceConfirmShare, which never checks clientSession.ByJwt.NetworkId. " +
				"Share confirm should be restricted to the source network.")
		}
	})
}

// A network that is not a party to an association cannot remove it.
func TestDeviceRemoveAssociationByUnrelatedNetworkRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		netA, _, _ := testingAdoptNetwork(ctx, "a")
		netC, _, _ := testingAdoptNetwork(ctx, "c")
		noAuth := session.Testing_CreateClientSession(ctx, nil)

		created, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{DeviceName: "d", DeviceSpec: "s"}, noAuth)
		assert.Equal(t, err, nil)
		_, err = DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netA)
		assert.Equal(t, err, nil)

		// C (unrelated) tries to remove A's pending adoption.
		removed, err := DeviceRemoveAssociation(
			&DeviceRemoveAssociationArgs{Code: created.AdoptCode}, netC)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, removed.Error, nil)

		// A still has the association.
		assoc, err := DeviceAssociations(netA)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(assoc.PendingAdoptionDevices), 1)
	})
}

// A network that is not a party to an association cannot rename it.
func TestDeviceSetAssociationNameByUnrelatedNetworkRejected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		netA, _, _ := testingAdoptNetwork(ctx, "a")
		netC, _, _ := testingAdoptNetwork(ctx, "c")
		noAuth := session.Testing_CreateClientSession(ctx, nil)

		created, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{DeviceName: "d", DeviceSpec: "s"}, noAuth)
		assert.Equal(t, err, nil)
		_, err = DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netA)
		assert.Equal(t, err, nil)

		result, err := DeviceSetAssociationName(
			&DeviceSetAssociationNameArgs{Code: created.AdoptCode, DeviceName: "hacked"}, netC)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.Error, nil)
	})
}
