// The owner's shared contract origin notifications (server CPUOPT6.md B #2):
// an owner hands one instance to every authority it creates, so a hosted device
// adds no notification goroutines, a device's Close leaves the instance to the
// owner's other devices, and a refusal recorded through one device replays for
// an identical request through another. Every identity is synthetic and no
// database or Redis is used: nothing here registers a watch, which would open
// a subscription, and authorities are built past their credential checks.
package localclient

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// the goroutines one owner runs: 16 bucket readers and 2 publishers
const originNotificationsTestOwnerGoroutineCount = 18

// A validated parent's claims, for building an authority past its credential
// checks.
func originNotificationsTestClaims() *session.ByJwt {
	return session.NewByJwt(server.NewId(), server.NewId(), "origin-notifications-test", false, false).Client(server.NewId(), server.NewId())
}

// An authority built as NewWithSettings builds one after validation.
func originNotificationsTestAuthority(ctx context.Context, settings *AuthoritySettings) (*Authority, *session.ByJwt) {
	claims := originNotificationsTestClaims()
	return newAuthority(ctx, claims, "", "https://control.example", settings), claims
}

// The notifications the authority's operations run with: the instance its
// session context carries, as the controller reads it.
func originNotificationsTestSessionNotifications(ctx context.Context, authority *Authority, claims *session.ByJwt) *model.ContractOriginNotifications {
	clientSession := authority.newSession(ctx, claims)
	defer clientSession.Cancel()
	return model.GetContractOriginNotifications(clientSession.Ctx)
}

// The goroutine count once it has held still for three reads 10 ms apart, so a
// goroutine of an earlier test that is still ending is not counted.
func originNotificationsTestGoroutines(t *testing.T) int {
	t.Helper()
	count := runtime.NumGoroutine()
	still := 0
	for deadline := time.Now().Add(5 * time.Second); still < 3; {
		if deadline.Before(time.Now()) {
			t.Fatalf("the goroutine count did not settle (last %d)", count)
		}
		time.Sleep(10 * time.Millisecond)
		next := runtime.NumGoroutine()
		if next == count {
			still += 1
		} else {
			count = next
			still = 0
		}
	}
	return count
}

// Authorities that share their owner's notifications add no goroutines however
// many there are, while each authority with its own adds one owner's 18, which
// its Close joins. On f43488da1 every hosted device built its own.
func TestAuthoritiesSharingTheOwnerNotificationsAddNoGoroutines(t *testing.T) {
	const authorityCount = 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
	defer owner.Close()

	before := originNotificationsTestGoroutines(t)
	shared := []*Authority{}
	for range authorityCount {
		authority, _ := originNotificationsTestAuthority(ctx, &AuthoritySettings{ContractOriginNotifications: owner})
		shared = append(shared, authority)
	}
	if grown := originNotificationsTestGoroutines(t) - before; grown != 0 {
		t.Fatalf("%d authorities sharing one owner's notifications added %d goroutines, want 0", authorityCount, grown)
	}
	for _, authority := range shared {
		authority.Close()
	}

	before = originNotificationsTestGoroutines(t)
	owned := []*Authority{}
	for range authorityCount {
		authority, _ := originNotificationsTestAuthority(ctx, &AuthoritySettings{})
		owned = append(owned, authority)
	}
	if grown := originNotificationsTestGoroutines(t) - before; grown != authorityCount*originNotificationsTestOwnerGoroutineCount {
		t.Fatalf("%d authorities with their own notifications added %d goroutines, want %d", authorityCount, grown, authorityCount*originNotificationsTestOwnerGoroutineCount)
	}
	for _, authority := range owned {
		authority.Close()
	}
	if grown := originNotificationsTestGoroutines(t) - before; grown != 0 {
		t.Fatalf("closing the authorities with their own notifications left %d goroutines", grown)
	}
}

// A device's Close leaves the owner's notifications to its other devices: a
// refusal recorded through one device's session still replays for an
// identical request through another device's after the first closes.
func TestAuthorityCloseLeavesTheSharedNotificationsLive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := model.NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings())
	defer owner.Close()
	settings := &AuthoritySettings{ContractOriginNotifications: owner}
	first, firstClaims := originNotificationsTestAuthority(ctx, settings)
	second, secondClaims := originNotificationsTestAuthority(ctx, settings)
	defer second.Close()

	firstNotifications := originNotificationsTestSessionNotifications(ctx, first, firstClaims)
	if firstNotifications != owner {
		t.Fatal("the device's operations do not run with its owner's notifications")
	}
	sourceId, destinationId := server.NewId(), server.NewId()
	rejection := model.ContractRouteRejection{
		Cause:          model.ContractRouteMissingOrigin,
		ResolutionPath: "test",
	}
	if rejection.Terminal() {
		t.Fatal("the fixture refusal is terminal; it must take the 60 s ring")
	}
	if !firstNotifications.RecordRouteRejection(sourceId, destinationId, true, rejection, nil) {
		t.Fatal("the refusal was not recorded")
	}

	first.Close()

	secondNotifications := originNotificationsTestSessionNotifications(ctx, second, secondClaims)
	if secondNotifications != owner {
		t.Fatal("the other device's operations do not run with its owner's notifications")
	}
	replayed, ok := secondNotifications.RouteRejection(sourceId, destinationId, true)
	if !ok {
		t.Fatal("after one device closed, the refusal it recorded no longer replays through the owner's other device")
	}
	if replayed.Cause != rejection.Cause {
		t.Fatalf("replayed cause %d, want %d", replayed.Cause, rejection.Cause)
	}
	// the request shape is part of the route
	if _, ok := secondNotifications.RouteRejection(sourceId, destinationId, false); ok {
		t.Fatal("a refusal of the companion shape replayed for the forward shape")
	}
}

// An authority that built its own notifications closes them with itself: what
// it recorded no longer replays.
func TestAuthorityCloseClosesItsOwnNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	authority, claims := originNotificationsTestAuthority(ctx, &AuthoritySettings{})
	notifications := originNotificationsTestSessionNotifications(ctx, authority, claims)
	if notifications == nil {
		t.Fatal("the authority's operations run without notifications")
	}
	sourceId, destinationId := server.NewId(), server.NewId()
	rejection := model.ContractRouteRejection{
		Cause:          model.ContractRouteMissingOrigin,
		ResolutionPath: "test",
	}
	if !notifications.RecordRouteRejection(sourceId, destinationId, true, rejection, nil) {
		t.Fatal("the refusal was not recorded")
	}
	if _, ok := notifications.RouteRejection(sourceId, destinationId, true); !ok {
		t.Fatal("the recorded refusal does not replay before the close")
	}
	authority.Close()
	if _, ok := notifications.RouteRejection(sourceId, destinationId, true); ok {
		t.Fatal("the authority's own notifications replay a refusal after its Close")
	}
	if notifications.RecordRouteRejection(sourceId, destinationId, true, rejection, nil) {
		t.Fatal("the authority's own notifications record after its Close")
	}
}
