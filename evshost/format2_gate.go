// §19.11's format2 gate at the evs block (EVS2-M10-05; §11.6's format row, D-OPS-10, D-PHASE-10): the engine reads
// it through ServiceHost.Gates, which sidecarhost.NewServiceHost builds on server's reader of
// config/<env>/sidecar.yml and renews every 60 s. This file adds no reader of its own, so the block and every other
// process answer one file by one rule: a network is in when `enabled` and listed, or `enabled` and its bucket is
// under `percent`; an absent or unparsable file reads as off; and `purge` and `drop` read observe unless exactly
// `on`, so every failure keeps content.
//
// A job in this process that deletes for format 2 (EVSB-M10-01's purge, EVSB-M10-03's drops) asks the same gate
// through Host.Format2Gate, never a copy of the file.

package evshost

import (
	"github.com/urnetwork/sidecar/evsservice"
)

// The format2 gate the block's engine holds its pushes to, read per call from the host's gates: which networks'
// pushes may carry format 2, and whether the purge and drop jobs delete or only observe. A host given no gates
// answers closed and observe.
func (self *Host) Format2Gate() evsservice.Format2Gate {
	return self.engine.Format2Gate()
}
