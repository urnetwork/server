package connect

import (
	"context"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"time"
)

func (self *Resident) runStreamAuthorizations(snapshots <-chan []model.StreamHop) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	hops := map[server.Id]model.StreamHop{}
	sent := map[server.Id]model.StreamAuthorization{}
	publish := func() {
		for id, hop := range hops {
			if self.ctx.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(self.ctx, 2*time.Second)
			grant, err := model.AuthorizeStream(ctx, id, server.NowUtc())
			cancel()
			if err != nil {
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
			self.sendListenerFrame(connect.RequireToFrameWithDefaultProtocolVersion(open))
			sent[id] = grant
		}
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case snapshot := <-snapshots:
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
