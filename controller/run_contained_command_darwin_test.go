// Verifies Darwin's process-state boundary independently of reaper scheduling.
package controller

import (
	"testing"

	"golang.org/x/sys/unix"
)

// Sleeping, stopped, and unknown states must not be mistaken for exited children.
func TestContainedProcessGroupDarwinIgnoresOnlyZombies(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		state   int8
		groupId int32
		running bool
	}{
		{name: "starting", state: 1, groupId: 100, running: true},
		{name: "runnable", state: 2, groupId: 100, running: true},
		{name: "sleeping", state: 3, groupId: 100, running: true},
		{name: "stopped", state: 4, groupId: 100, running: true},
		{name: "zombie", state: 5, groupId: 100},
		{name: "unknown", state: 0, groupId: 100, running: true},
		{name: "other group", state: 2, groupId: 101},
	} {
		processes := []unix.KinfoProc{{
			Proc:  unix.ExternProc{P_stat: testCase.state},
			Eproc: unix.Eproc{Pgid: testCase.groupId},
		}}
		if running := containedDarwinProcessGroupHasLiveMembers(100, processes); running != testCase.running {
			t.Errorf("%s running = %v, want %v", testCase.name, running, testCase.running)
		}
	}
	processes := []unix.KinfoProc{
		{Proc: unix.ExternProc{P_stat: 5}, Eproc: unix.Eproc{Pgid: 100}},
		{Proc: unix.ExternProc{P_stat: 3}, Eproc: unix.Eproc{Pgid: 100}},
	}
	if !containedDarwinProcessGroupHasLiveMembers(100, processes) {
		t.Fatal("a zombie masked the group's live descendant")
	}
	if containedDarwinProcessGroupHasLiveMembers(100, nil) {
		t.Fatal("an empty process group is running")
	}
}
