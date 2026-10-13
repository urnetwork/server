// The owner's registration bound refuses before any credential or database
// read, reads as a rate limit to the SDK, and never leaks a slot. Every
// identity here is synthetic and no database is needed.
package localclient

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// An authority with no credential: any call that gets past admission fails
// its local preflight with a 401 and never reaches the database.
func mintAdmissionTestAuthority(admission *MintAdmission) (*Authority, *connect.AuthNetworkClientArgs) {
	clientId := server.NewId()
	parent := connect.Id(clientId)
	authority := &Authority{
		networkId:     server.NewId(),
		userId:        server.NewId(),
		clientId:      clientId,
		deviceId:      server.NewId(),
		apiUrl:        "https://control.example",
		mintAdmission: admission,
		cancel:        func() {},
	}
	return authority, &connect.AuthNetworkClientArgs{SourceClientId: &parent}
}

func mintAdmissionStatus(t *testing.T, err error) int {
	t.Helper()
	var status *connect.HttpStatusError
	if !errors.As(err, &status) {
		t.Fatalf("error %v carries no http status", err)
	}
	return status.StatusCode
}

// A full bound refuses at once as a 429 the SDK window classifier reads as a
// rate limit; a free slot admits the call to its preflight, and the slot is
// returned when the call ends.
func TestAuthorityMintAdmissionRefusesPastBound(t *testing.T) {
	admission := NewMintAdmission(1)
	authority, args := mintAdmissionTestAuthority(admission)
	if !admission.tryAcquire() {
		t.Fatal("a fresh bound refused its first slot")
	}
	_, err := authority.AuthNetworkClient(t.Context(), args)
	if mintAdmissionStatus(t, err) != http.StatusTooManyRequests {
		t.Fatalf("a full bound answered %v, want 429", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "429") {
		t.Fatalf("refusal %q does not read as a rate limit", err)
	}
	admission.release()

	// admitted: the empty credential fails preflight, not admission
	_, err = authority.AuthNetworkClient(t.Context(), args)
	if mintAdmissionStatus(t, err) != http.StatusUnauthorized {
		t.Fatalf("an admitted call answered %v, want its preflight 401", err)
	}
	if !admission.tryAcquire() {
		t.Fatal("an ended call kept its slot")
	}
	admission.release()
}

// One owner's bound is shared by all of its authorities: the slots taken
// through one device are unavailable to another.
func TestAuthorityMintAdmissionIsSharedByOwner(t *testing.T) {
	admission := NewMintAdmission(2)
	first, firstArgs := mintAdmissionTestAuthority(admission)
	second, secondArgs := mintAdmissionTestAuthority(admission)
	for range 2 {
		if !admission.tryAcquire() {
			t.Fatal("the bound refused below its capacity")
		}
	}
	for _, c := range []struct {
		authority *Authority
		args      *connect.AuthNetworkClientArgs
	}{
		{authority: first, args: firstArgs},
		{authority: second, args: secondArgs},
	} {
		if _, err := c.authority.AuthNetworkClient(t.Context(), c.args); mintAdmissionStatus(t, err) != http.StatusTooManyRequests {
			t.Fatalf("a device of a full owner answered %v, want 429", err)
		}
	}
	admission.release()
	admission.release()
}

// Concurrent callers never hold more slots than the bound, and every admitted
// call returns its slot.
func TestAuthorityMintAdmissionConcurrentCallsReturnSlots(t *testing.T) {
	admission := NewMintAdmission(3)
	authority, args := mintAdmissionTestAuthority(admission)
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := authority.AuthNetworkClient(context.Background(), args)
			var status *connect.HttpStatusError
			if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized && status.StatusCode != http.StatusTooManyRequests {
				t.Errorf("unexpected result %v", err)
			}
		}()
	}
	wait.Wait()
	for range 3 {
		if !admission.tryAcquire() {
			t.Fatal("a slot leaked after concurrent calls")
		}
	}
	if admission.tryAcquire() {
		t.Fatal("the bound admitted past its capacity")
	}
}

// No admission admits everything, and nonpositive capacities build none.
func TestAuthorityMintAdmissionNilAdmitsEverything(t *testing.T) {
	if NewMintAdmission(0) != nil || NewMintAdmission(-1) != nil {
		t.Fatal("a nonpositive bound built an admission")
	}
	authority, args := mintAdmissionTestAuthority(nil)
	for range 4 {
		if _, err := authority.AuthNetworkClient(t.Context(), args); mintAdmissionStatus(t, err) != http.StatusUnauthorized {
			t.Fatalf("an unbounded owner answered %v, want its preflight 401", err)
		}
	}
}

// A closed authority reports its lifecycle, not a rate limit.
func TestAuthorityMintAdmissionClosedAuthorityIsCanceled(t *testing.T) {
	admission := NewMintAdmission(1)
	authority, args := mintAdmissionTestAuthority(admission)
	authority.closed.Store(true)
	if _, err := authority.AuthNetworkClient(t.Context(), args); !errors.Is(err, context.Canceled) {
		t.Fatalf("a closed authority answered %v, want canceled", err)
	}
	if !admission.tryAcquire() {
		t.Fatal("a closed authority consumed a slot")
	}
}
