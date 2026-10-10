// Registered function lanes cannot be hidden by another function's older
// backlog. One indexed lane turn alternates with the original global queue.
package task

// Rotation is shared by this worker's Run callers. An isolated lane handed
// from refill to the next initial claim belongs only to that Run's poll state.
func (self *TaskWorker) nextClaimFunction(options taskClaimOptions) string {
	if !self.settings.FairClaimFunctions || options.poll == nil {
		return ""
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.draining || self.runCtx.Err() != nil || len(self.claimFunctionNames) == 0 {
		return ""
	}
	if !options.ordinaryOnly && options.poll.isolatedFunction != "" {
		name := options.poll.isolatedFunction
		options.poll.isolatedFunction = ""
		return name
	}
	turn := self.claimFunctionTurn
	self.claimFunctionTurn++
	if turn%2 != 0 {
		return ""
	}
	name := self.claimFunctionNames[(turn/2)%uint64(len(self.claimFunctionNames))]
	canonical := self.claimTargetName(name)
	if limit := self.settings.TargetClaimLimits[canonical]; limit != 0 && limit <= self.claimTargetCounts[canonical] {
		return ""
	}
	return name
}
