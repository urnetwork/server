package connect

// What a test outside this package does to a resident: sidecart3's disruption rows (TEST-M2-15; §22.8's resident
// migration and 60 s ack timeout) kill one as its host's crash or a move's retirement would. Production code calls none
// of it.

import (
	"github.com/urnetwork/server"
)

// Cancels clientId's resident on this exchange, as a resident whose host died ends, and answers whether there was one.
// The client's next transport finds no resident and nominates a new one.
func (self *Exchange) Testing_CancelLocalResident(clientId server.Id) bool {
	resident := self.localResident(clientId)
	if resident == nil {
		return false
	}
	resident.Cancel()
	return true
}
