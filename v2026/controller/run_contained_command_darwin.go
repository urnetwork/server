// Inspects the owned process group through Darwin's native process table, not procfs.
package controller

import "golang.org/x/sys/unix"

// A surviving child can retain its group after the leader exits; zombies do not run.
func containedProcessGroupRunning(processGroupId int) (bool, error) {
	// Darwin's group signal probe returns EPERM for zombie-only groups; the
	// native process table, not signal permission, determines liveness.
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", processGroupId)
	if err != nil {
		return false, err
	}
	return containedDarwinProcessGroupHasLiveMembers(processGroupId, processes), nil
}

// Counts every matching state except Darwin's SZOMB (sys/proc.h), including stopped children.
func containedDarwinProcessGroupHasLiveMembers(processGroupId int, processes []unix.KinfoProc) bool {
	const zombieState = 5
	for _, process := range processes {
		if int(process.Eproc.Pgid) == processGroupId && process.Proc.P_stat != zombieState {
			return true
		}
	}
	return false
}
