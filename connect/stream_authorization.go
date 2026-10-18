// A replacement resident reconciles the first complete authorized hop snapshot.
package connect

import (
	"context"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Authorization failures defer the initial reset so surviving streams are not
// retired by a partial read. Later failures remain bounded by receiver leases.
func (self *Resident) runStreamAuthorizations(snapshots <-chan []model.StreamHop) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	hops := map[server.Id]model.StreamHop{}
	sent := map[server.Id]model.StreamAuthorization{}
	haveSnapshot := false
	initialized := false
	publish := func() {
		if !haveSnapshot {
			return
		}
		initialStreams := []*protocol.StreamOpen{}
		initialGrants := map[server.Id]model.StreamAuthorization{}
		for id, hop := range hops {
			if self.ctx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
			grant, err := model.AuthorizeStream(ctx, id, server.NowUtc())
			cancel()
			if err != nil {
				if !initialized {
					return
				}
				continue
			} // receiver timers enforce dependency failure
			if !grant.Allowed {
				if old, ok := sent[id]; ok {
					self.sendListenerFrame(connect.RequireToFrameWithDefaultProtocolVersion(&protocol.StreamAuthorization{StreamId: id.Bytes(), AuthorizationGeneration: old.Generation.Bytes(), Retired: true}))
					delete(sent, id)
				}
				continue
			}
			open := streamHopToProtocol(hop)
			if !grant.Legacy {
				open.AuthorizationGeneration = grant.Generation.Bytes()
				open.AuthorizationLeaseMillis = grant.LeaseMillis
				open.AuthorizationDeadlineUnixMillis = grant.DeadlineUnixMillis
			}
			if initialized {
				self.sendListenerFrame(connect.RequireToFrameWithDefaultProtocolVersion(open))
				sent[id] = grant
			} else {
				initialStreams = append(initialStreams, open)
				initialGrants[id] = grant
			}
		}
		if !initialized {
			self.sendListenerFrame(connect.RequireToFrameWithDefaultProtocolVersion(&protocol.StreamReset{Streams: initialStreams}))
			sent = initialGrants
			initialized = true
		}
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case snapshot, ok := <-snapshots:
			if !ok {
				return
			}
			haveSnapshot = true
			next := map[server.Id]model.StreamHop{}
			for _, hop := range snapshot {
				next[hop.StreamId()] = hop
			}
			for id, old := range sent {
				if _, ok := next[id]; !ok {
					self.sendListenerFrame(connect.RequireToFrameWithDefaultProtocolVersion(&protocol.StreamClose{StreamId: id.Bytes(), AuthorizationGeneration: old.Generation.Bytes()}))
					delete(sent, id)
				}
			}
			hops = next
			publish()
		case <-ticker.C:
			publish()
		}
	}
}
