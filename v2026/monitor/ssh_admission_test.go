// Synthetic demands prove admission fairness without SSH, production identity,
// sleeps, or an assumed relation between signal slots and command fanout.
package monitor

import (
	"fmt"
	"slices"
	"testing"
)

func syntheticSshAdmissionOwner(pid int) sshAdmissionOwner {
	return sshAdmissionOwner{Identity: SshAdmissionWatcher{Pid: pid, StartTicks: uint64(pid), BootId: "00000000-0000-0000-0000-000000000001"}, Unit: fmt.Sprintf("synthetic-%d.service", pid), InvocationId: fmt.Sprintf("%032x", pid), Cgroup: fmt.Sprintf("/synthetic/synthetic-%d.service", pid), Device: 1, Inode: uint64(pid)}
}

func syntheticSshAdmissionState() sshAdmissionState {
	owner := syntheticSshAdmissionOwner(101)
	return sshAdmissionState{Version: 1, BootId: owner.Identity.BootId, Watcher: owner, Hosts: []string{"a.example", "b.example", "c.example", "d.example", "e.example"}, NextTicket: 1, Turn: sshAdmissionDiagnostic}
}

func appendSyntheticSshAdmission(state *sshAdmissionState, class string, hosts ...string) string {
	owner := state.Watcher
	if class == sshAdmissionDiagnostic {
		owner = syntheticSshAdmissionOwner(202)
	}
	token := fmt.Sprintf("%032x", state.NextTicket)
	hosts = slices.Clone(hosts)
	slices.Sort(hosts)
	state.Requests = append(state.Requests, sshAdmissionRequest{Token: token, Ticket: state.NextTicket, Class: class, Owner: owner, Hosts: hosts, ExpiresNanos: 100})
	state.NextTicket++
	return token
}

func removeSyntheticSshAdmission(state *sshAdmissionState, token string) {
	for i, request := range state.Requests {
		if request.Token == token {
			state.Requests = slices.Delete(state.Requests, i, i+1)
			return
		}
	}
}

func TestSharedSshAdmissionFanoutCannotStarveReservedHop(t *testing.T) {
	state := syntheticSshAdmissionState()
	active := []string{}
	for _, host := range []string{"a.example", "b.example", "c.example", "d.example"} {
		token := appendSyntheticSshAdmission(&state, sshAdmissionWatch, host)
		if !state.grant(token) {
			t.Fatal("healthy fanout did not fill four slots")
		}
		active = append(active, token)
	}
	hop := appendSyntheticSshAdmission(&state, sshAdmissionDiagnostic, "a.example", "e.example")
	refill := appendSyntheticSshAdmission(&state, sshAdmissionWatch, "b.example")
	removeSyntheticSshAdmission(&state, active[0])
	if state.grant(refill) || state.grant(hop) {
		t.Fatal("fanout refilled a reserved hop slot or hop was partially granted")
	}
	removeSyntheticSshAdmission(&state, active[1])
	if state.next() != hop || !state.grant(hop) {
		t.Fatal("two-slot diagnostic remained starved by continuing watcher fanout")
	}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
	removeSyntheticSshAdmission(&state, active[2])
	if !state.grant(refill) {
		t.Fatal("diagnostic prevented concurrent watcher progress in remaining capacity")
	}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
	removeSyntheticSshAdmission(&state, hop)
	nextDiagnostic := appendSyntheticSshAdmission(&state, sshAdmissionDiagnostic, "a.example", "e.example")
	watcher := appendSyntheticSshAdmission(&state, sshAdmissionWatch, "c.example")
	// The preceding watcher already earned its turn, so this diagnostic may
	// go first; its grant must hand the next available turn back to the watcher.
	if !state.grant(nextDiagnostic) {
		t.Fatal("finite subsequent diagnostic was refused")
	}
	removeSyntheticSshAdmission(&state, nextDiagnostic)
	third := appendSyntheticSshAdmission(&state, sshAdmissionDiagnostic, "a.example")
	if state.next() != watcher || state.grant(third) || !state.grant(watcher) {
		t.Fatal("diagnostics starved ordinary work")
	}
}

func TestSharedSshAdmissionHostLimitAndBusyNeighbor(t *testing.T) {
	state := syntheticSshAdmissionState()
	for range 2 {
		token := appendSyntheticSshAdmission(&state, sshAdmissionWatch, "a.example")
		if !state.grant(token) {
			t.Fatal("healthy host refused")
		}
	}
	blocked := appendSyntheticSshAdmission(&state, sshAdmissionWatch, "a.example")
	other := appendSyntheticSshAdmission(&state, sshAdmissionWatch, "b.example")
	if state.grant(blocked) || !state.grant(other) {
		t.Fatal("host limit leaked or busy host blocked an unrelated destination")
	}
	if err := state.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedSshAdmissionExpiryNeverReleasesActiveWork(t *testing.T) {
	for _, status := range []sshAdmissionOwnerStatus{sshAdmissionOwnerUnknown, sshAdmissionOwnerLive, sshAdmissionOwnerDead, sshAdmissionOwnerGone} {
		state := syntheticSshAdmissionState()
		active := appendSyntheticSshAdmission(&state, sshAdmissionWatch, "a.example")
		if !state.grant(active) {
			t.Fatal("setup grant")
		}
		appendSyntheticSshAdmission(&state, sshAdmissionWatch, "b.example")
		state.prune(1000, map[sshAdmissionOwner]sshAdmissionOwnerStatus{state.Watcher: status})
		want := 1
		if status == sshAdmissionOwnerGone {
			want = 0
		}
		if len(state.Requests) != want || (want == 1 && state.Requests[0].Token != active) {
			t.Fatal("expiry or dead process released possible active SSH children")
		}
	}
}

func TestSharedSshAdmissionCanceledTurnAndMalformedState(t *testing.T) {
	state := syntheticSshAdmissionState()
	hop := appendSyntheticSshAdmission(&state, sshAdmissionDiagnostic, "a.example", "b.example")
	watch := appendSyntheticSshAdmission(&state, sshAdmissionWatch, "c.example")
	removeSyntheticSshAdmission(&state, hop)
	if !state.grant(watch) {
		t.Fatal("removed diagnostic retained its fair turn")
	}
	for _, corrupt := range []func(*sshAdmissionState){
		func(state *sshAdmissionState) { state.Requests[0].Hosts = []string{"outside.example"} },
		func(state *sshAdmissionState) { state.Requests[0].Hosts = []string{"a.example", "a.example"} },
		func(state *sshAdmissionState) { state.Requests[0].Ticket = state.NextTicket },
		func(state *sshAdmissionState) { state.Requests[0].Owner.Inode = 0 },
		func(state *sshAdmissionState) { state.Turn = "unknown" },
		func(state *sshAdmissionState) { state.Hosts = append(state.Hosts, state.Hosts[0]) },
	} {
		bad := state
		bad.Requests = slices.Clone(state.Requests)
		bad.Hosts = slices.Clone(state.Hosts)
		corrupt(&bad)
		if bad.validate() == nil {
			t.Fatal("malformed reservation became capacity")
		}
	}
}
