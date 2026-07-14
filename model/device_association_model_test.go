package model

// basic test for each function

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result1.Error, nil)
		connect.AssertNotEqual(t, result1.AdoptCode, "")
		connect.AssertNotEqual(t, result1.AdoptSecret, "")

		qrResult0, err := DeviceAdoptCodeQR(
			&DeviceAdoptCodeQRArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, qrResult0.PngBytes, nil)

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult0.PendingAdoptionDevices), 1)
		connect.AssertEqual(t, len(associationResult0.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult0.OutgoingSharedDevices), 0)

		result4, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             result1.AdoptCode,
				AdoptSecret:           result1.AdoptSecret,
				AssociatedNetworkName: result3.AssociatedNetworkName,
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result4.Error, nil)
		// connect.AssertEqual(t, result4.AssociatedNetworkName, "a")
		connect.AssertNotEqual(t, result4.ByClientJwt, "")
		byJwt, err := jwt.ParseByJwt(ctx, result4.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, byJwt.NetworkId, networkIdA)
		connect.AssertEqual(t, byJwt.NetworkName, "a")
		connect.AssertEqual(t, byJwt.UserId, userIdA)
		connect.AssertEqual(t, byJwt.GuestMode, false)

		// at this point there should be no adopt association
		associationResult1, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult1.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult1.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult1.OutgoingSharedDevices), 0)

		clientResult1, err := GetNetworkClients(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientResult1.Clients), 2)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, created.Error, nil)
		connect.AssertNotEqual(t, created.AdoptCode, "")
		connect.AssertNotEqual(t, created.AdoptSecret, "")

		// Step 3: the victim adopts the (public) code.
		added, err := DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, victimSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, added.Error, nil)

		// Step 4: unauthenticated adopt-status leaks the victim's network name.
		status, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{AdoptCode: created.AdoptCode},
			noAuthSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, status.Error, nil)
		connect.AssertEqual(t, status.AssociatedNetworkName, "a")

		// Step 5: confirm with the leaked network name but a DELIBERATELY WRONG
		// secret. A legitimate device proves possession of adopt_secret here; an
		// attacker who only eavesdropped the code does not have it.
		wrongSecret := "00000000000000000000000000000000deadbeefdeadbeef"
		connect.AssertNotEqual(t, wrongSecret, created.AdoptSecret)

		confirmed, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             created.AdoptCode,
				AdoptSecret:           wrongSecret,
				AssociatedNetworkName: status.AssociatedNetworkName,
			},
			noAuthSession,
		)
		connect.AssertEqual(t, err, nil)

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
		connect.AssertNotEqual(t, confirmed.Error, nil)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result1.Error, nil)
		connect.AssertNotEqual(t, result1.AdoptCode, "")
		connect.AssertNotEqual(t, result1.AdoptSecret, "")

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult0.PendingAdoptionDevices), 1)
		connect.AssertEqual(t, len(associationResult0.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult0.OutgoingSharedDevices), 0)

		removeResult0, err := DeviceRemoveAdoptCode(
			&DeviceRemoveAdoptCodeArgs{
				AdoptCode:   result1.AdoptCode,
				AdoptSecret: result1.AdoptSecret,
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult0.Error, nil)

		associationResult1, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult1.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult1.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult1.OutgoingSharedDevices), 0)

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientsResult0.Clients), 1)

		result1, err := DeviceCreateAdoptCode(
			&DeviceCreateAdoptCodeArgs{
				DeviceName: "devicec",
				DeviceSpec: "specc",
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result1.Error, nil)
		connect.AssertNotEqual(t, result1.AdoptCode, "")
		connect.AssertNotEqual(t, result1.AdoptSecret, "")

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Error, nil)

		result3, err := DeviceAdoptStatus(
			&DeviceAdoptStatusArgs{
				AdoptCode: result1.AdoptCode,
			},
			clientSessionNoAuth,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result3.Error, nil)

		// at this point there should be adopt association
		associationResult0, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult0.PendingAdoptionDevices), 1)
		connect.AssertEqual(t, len(associationResult0.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult0.OutgoingSharedDevices), 0)

		removeResult0, err := DeviceRemoveAssociation(
			&DeviceRemoveAssociationArgs{
				Code: result1.AdoptCode,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult0.Error, nil)

		associationResult1, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult1.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult1.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult1.OutgoingSharedDevices), 0)

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientsResult0.Clients), 1)

		clientsResult1, err := GetNetworkClients(clientSessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientsResult1.Clients), 1)

		result1, err := DeviceCreateShareCode(
			&DeviceCreateShareCodeArgs{
				ClientId:   clientIdA,
				DeviceName: "devicea",
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result1.Error, nil)
		connect.AssertNotEqual(t, result1.ShareCode, "")

		associationResult0, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult0.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult0.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult0.OutgoingSharedDevices), 1)
		connect.AssertEqual(t, associationResult0.OutgoingSharedDevices[0].Pending, true)
		connect.AssertEqual(t, associationResult0.OutgoingSharedDevices[0].NetworkName, "")
		connect.AssertEqual(t, associationResult0.OutgoingSharedDevices[0].DeviceName, "devicea")

		shareStatus1, err := DeviceShareStatus(
			&DeviceShareStatusArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, shareStatus1.Error, nil)

		qrResult0, err := DeviceShareCodeQR(
			&DeviceShareCodeQRArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, qrResult0.PngBytes, nil)

		result2, err := DeviceAdd(
			&DeviceAddArgs{
				Code: result1.ShareCode,
			},
			clientSessionB,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result2.Error, nil)

		result3, err := DeviceShareStatus(
			&DeviceShareStatusArgs{
				ShareCode: result1.ShareCode,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result3.Error, nil)

		// at this point there should be share association pending

		associationResult1, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult1.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult1.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult1.OutgoingSharedDevices), 1)
		connect.AssertEqual(t, associationResult1.OutgoingSharedDevices[0].Pending, true)
		connect.AssertEqual(t, associationResult1.OutgoingSharedDevices[0].NetworkName, "b")
		connect.AssertEqual(t, associationResult1.OutgoingSharedDevices[0].DeviceName, "devicea")

		associationResult2, err := DeviceAssociations(clientSessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult2.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult2.IncomingSharedDevices), 1)
		connect.AssertEqual(t, len(associationResult2.OutgoingSharedDevices), 0)
		connect.AssertEqual(t, associationResult2.IncomingSharedDevices[0].Pending, true)
		connect.AssertEqual(t, associationResult2.IncomingSharedDevices[0].NetworkName, "a")
		connect.AssertEqual(t, associationResult2.IncomingSharedDevices[0].DeviceName, "devicea")

		result4, err := DeviceConfirmShare(
			&DeviceConfirmShareArgs{
				ShareCode:             result1.ShareCode,
				AssociatedNetworkName: result3.AssociatedNetworkName,
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result4.Error, nil)
		connect.AssertEqual(t, result4.AssociatedNetworkName, "b")

		// at this point there should be a share association not pending, as outgoing for A, incoming for B
		associationResult3, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult3.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult3.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult3.OutgoingSharedDevices), 1)
		connect.AssertEqual(t, associationResult3.OutgoingSharedDevices[0].Pending, false)
		connect.AssertEqual(t, associationResult3.OutgoingSharedDevices[0].NetworkName, "b")
		connect.AssertEqual(t, associationResult3.OutgoingSharedDevices[0].DeviceName, "devicea")

		associationResult4, err := DeviceAssociations(clientSessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult4.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult4.IncomingSharedDevices), 1)
		connect.AssertEqual(t, len(associationResult4.OutgoingSharedDevices), 0)
		connect.AssertEqual(t, associationResult4.IncomingSharedDevices[0].Pending, false)
		connect.AssertEqual(t, associationResult4.IncomingSharedDevices[0].NetworkName, "a")
		connect.AssertEqual(t, associationResult4.IncomingSharedDevices[0].DeviceName, "devicea")

		setNameResult1, err := DeviceSetAssociationName(
			&DeviceSetAssociationNameArgs{
				Code:       result1.ShareCode,
				DeviceName: "That device I shared",
			},
			clientSessionA,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, setNameResult1.Error, nil)

		setNameResult2, err := DeviceSetAssociationName(
			&DeviceSetAssociationNameArgs{
				Code:       result1.ShareCode,
				DeviceName: "My new device from friend",
			},
			clientSessionB,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, setNameResult2.Error, nil)

		associationResult5, err := DeviceAssociations(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult5.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult5.IncomingSharedDevices), 0)
		connect.AssertEqual(t, len(associationResult5.OutgoingSharedDevices), 1)
		connect.AssertEqual(t, associationResult5.OutgoingSharedDevices[0].Pending, false)
		connect.AssertEqual(t, associationResult5.OutgoingSharedDevices[0].NetworkName, "b")
		connect.AssertEqual(t, associationResult5.OutgoingSharedDevices[0].DeviceName, "That device I shared")

		associationResult6, err := DeviceAssociations(clientSessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(associationResult6.PendingAdoptionDevices), 0)
		connect.AssertEqual(t, len(associationResult6.IncomingSharedDevices), 1)
		connect.AssertEqual(t, len(associationResult6.OutgoingSharedDevices), 0)
		connect.AssertEqual(t, associationResult6.IncomingSharedDevices[0].Pending, false)
		connect.AssertEqual(t, associationResult6.IncomingSharedDevices[0].NetworkName, "a")
		connect.AssertEqual(t, associationResult6.IncomingSharedDevices[0].DeviceName, "My new device from friend")

		clientsResult2, err := GetNetworkClients(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientsResult2.Clients), 1)

		clientsResult3, err := GetNetworkClients(clientSessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientsResult3.Clients), 1)
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
		connect.AssertEqual(t, err, nil)

		added, err := DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, victim)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, added.Error, nil)

		confirmed, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             created.AdoptCode,
				AdoptSecret:           created.AdoptSecret,
				AssociatedNetworkName: "some-other-network",
			}, noAuth)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, confirmed.ByClientJwt, "")
		connect.AssertNotEqual(t, confirmed.Error, nil)
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
		connect.AssertEqual(t, err, nil)

		confirmed, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             created.AdoptCode,
				AdoptSecret:           created.AdoptSecret,
				AssociatedNetworkName: "a",
			}, noAuth)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, confirmed.ByClientJwt, "")
		connect.AssertNotEqual(t, confirmed.Error, nil)
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
		connect.AssertEqual(t, err, nil)

		_, err = DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, victim)
		connect.AssertEqual(t, err, nil)

		args := &DeviceConfirmAdoptArgs{
			AdoptCode:             created.AdoptCode,
			AdoptSecret:           created.AdoptSecret,
			AssociatedNetworkName: "a",
		}

		first, err := DeviceConfirmAdopt(args, noAuth)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, first.ByClientJwt, "")

		clientsAfterFirst, err := GetNetworkClients(victim)
		connect.AssertEqual(t, err, nil)

		second, err := DeviceConfirmAdopt(args, noAuth)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, second.ByClientJwt, "")
		connect.AssertNotEqual(t, second.Error, nil)

		clientsAfterSecond, err := GetNetworkClients(victim)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(clientsAfterSecond.Clients), len(clientsAfterFirst.Clients))
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
		connect.AssertEqual(t, err, nil)

		addedA, err := DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, addedA.Error, nil)

		// B tries to adopt the same, already-owned code.
		addedB, err := DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netB)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, addedB.Error, nil)

		// The device confirms for A (the true owner); the JWT is bound to A, not B.
		confirmed, err := DeviceConfirmAdopt(
			&DeviceConfirmAdoptArgs{
				AdoptCode:             created.AdoptCode,
				AdoptSecret:           created.AdoptSecret,
				AssociatedNetworkName: "a",
			}, noAuth)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, confirmed.ByClientJwt, "")
		byJwt, err := jwt.ParseByJwt(ctx, confirmed.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, byJwt.NetworkId, networkIdA)
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
		connect.AssertEqual(t, err, nil)

		_, err = DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netA)
		connect.AssertEqual(t, err, nil)

		// Wrong secret: must not remove.
		wrong, err := DeviceRemoveAdoptCode(
			&DeviceRemoveAdoptCodeArgs{AdoptCode: created.AdoptCode, AdoptSecret: "wrong-secret"}, noAuth)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, wrong.Error, nil)

		// Still present: A still sees a pending adoption.
		assoc, err := DeviceAssociations(netA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(assoc.PendingAdoptionDevices), 1)

		// Correct secret: removes.
		ok, err := DeviceRemoveAdoptCode(
			&DeviceRemoveAdoptCodeArgs{AdoptCode: created.AdoptCode, AdoptSecret: created.AdoptSecret}, noAuth)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ok.Error, nil)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)
		connect.AssertEqual(t, result.ShareCode, "")
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, share.Error, nil)

		added, err := DeviceAdd(&DeviceAddArgs{Code: share.ShareCode}, netA)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, added.Error, nil)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, share.Error, nil)

		added, err := DeviceAdd(&DeviceAddArgs{Code: share.ShareCode}, netB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, added.Error, nil)

		// C, knowing only the share code and the guest's network name, confirms.
		confirmed, err := DeviceConfirmShare(
			&DeviceConfirmShareArgs{ShareCode: share.ShareCode, AssociatedNetworkName: "b"}, netC)
		connect.AssertEqual(t, err, nil)
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
		connect.AssertEqual(t, err, nil)
		_, err = DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netA)
		connect.AssertEqual(t, err, nil)

		// C (unrelated) tries to remove A's pending adoption.
		removed, err := DeviceRemoveAssociation(
			&DeviceRemoveAssociationArgs{Code: created.AdoptCode}, netC)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, removed.Error, nil)

		// A still has the association.
		assoc, err := DeviceAssociations(netA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(assoc.PendingAdoptionDevices), 1)
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
		connect.AssertEqual(t, err, nil)
		_, err = DeviceAdd(&DeviceAddArgs{Code: created.AdoptCode}, netA)
		connect.AssertEqual(t, err, nil)

		result, err := DeviceSetAssociationName(
			&DeviceSetAssociationNameArgs{Code: created.AdoptCode, DeviceName: "hacked"}, netC)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)
	})
}
