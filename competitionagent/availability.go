// Physical availability is a signed finite observation, independent of a
// historical qualification. Unknown cleanup never starts another job's clock.
package competitionagent

import (
	"context"
	"errors"
	"os"
)

// A terminal result alone cannot release a host: independent scoped cleanup
// and the service-manager join must both have completed before the next job.
func availableRunState(state *runState) bool {
	return state != nil && terminalState(state.State) && state.CleanupVerified && (!state.LaunchAttempted || state.StopCleanupVerified)
}

// Service inspection never retains the publication lock. A final state/slot
// read refuses concurrent changes instead of treating an earlier read as a lease.
// No request selects a run, unit, command or file at this boundary.
func (self *Agent) availability(ctx context.Context) (Response, error) {
	response := Response{Schema: Schema, HostId: self.config.HostId, State: "occupied", ExitCode: 0}
	lock, err := self.lock(ctx)
	if err != nil {
		return Response{}, err
	}
	slot, err := self.loadSlot()
	if errors.Is(err, os.ErrNotExist) || err == nil && slot.RunId == "" {
		lock.Close()
		response.State = "available"
		return response, nil
	}
	if err != nil {
		lock.Close()
		response.State, response.ExitCode, response.ErrorCode = "unavailable", -1, "physical_slot_unreadable"
		return response, nil
	}
	state, err := self.loadState(slot.RunId)
	lock.Close()
	if err != nil {
		return response, nil
	}
	if !availableRunState(state) {
		// A hard-exited evaluator may have joined without committing its
		// terminal result. Reconcile only that existing run through ordinary
		// status; it never launches, advances a fence or replaces its clock.
		if _, err := self.status(ctx, Request{Schema: Schema, HostId: self.config.HostId, RunId: slot.RunId, FencingToken: state.FencingToken}); err != nil {
			return response, nil
		}
		state, err = self.loadState(slot.RunId)
		if err != nil || !availableRunState(state) {
			return response, nil
		}
	}
	if state.LaunchAttempted {
		controlCtx, cancel := context.WithTimeout(ctx, self.settings.ControlTimeout)
		unit, err := self.runner.Status(controlCtx, unitName(slot.RunId))
		cancel()
		if err != nil || unit.Active {
			return response, nil
		}
	}
	lock, err = self.lock(ctx)
	if err != nil {
		return Response{}, err
	}
	defer lock.Close()
	current, err := self.loadSlot()
	if err != nil || current.RunId != slot.RunId {
		return response, nil
	}
	state, err = self.loadState(current.RunId)
	if err == nil && availableRunState(state) {
		response.State = "available"
	}
	return response, nil
}
