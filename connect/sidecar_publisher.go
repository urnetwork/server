package connect

// §6.8's resident publishers (PROTO-M1-09, D-PROTO-5): a resident of a client whose owner row marks it
// strict_exp, which only sidecars' clients carry (§7.4), runs one sidecar/pushservice.SidecarPublisher
// for its life, so the client hears its owners' hints from ControlId with no request of its own. The
// exchange's one shared key-event subscriber watches every client's {scp_<C>}n and kicks the
// client's publisher on its INCR, never a subscription per client (server/model/peer_model.go's record
// of the 2026-07-15 outage).
//
// The gate. EnableSidecarPush is sidecar.yml's push switch as the connect service read it at start
// (run.go): it alone subscribes the {scp_*}n pattern, since every keyspace notification on every master
// is matched against every pattern, so a gate turned on applies at the process's next start. A resident
// starts a publisher only while the switch still reads on, so turning it off stops new publishers
// within the file's reread. The binary must link MG0, whose network_client_owner holds the mark; the
// mark is read once, with the resident's profile (plan-PROTO-small-readings reading 6), since a
// resident is built from a client id and never sees the token.
//
// Drain, moves and migration need nothing of their own: a new resident starts a new publisher, whose
// reset is what resyncs the client. PROTO-M8-01 adds acl_enforcing device clients, told team alone,
// and {scp_<C>}live.

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var (
	sidecarPublishersStarted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_sidecar_publishers_started_total",
		Help: "resident publishers started for strict_exp clients (§6.8)",
	})
	sidecarPublisherSettingsRefused = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "urnetwork_sidecar_publisher_settings_refused_total",
		Help: "resident publishers not started because the exchange's publisher settings were refused",
	})
)

func init() {
	prometheus.MustRegister(
		sidecarPublishersStarted,
		sidecarPublisherSettingsRefused,
	)
}

// The switch and tunables of resident publishers. Embedded in ExchangeSettings.
type ExchangeSidecarPushSettings struct {
	// sidecar.yml's push switch as the process read it at start, and the ceiling: false subscribes no
	// {scp_*}n pattern, reads no mark and starts no publisher. Off by default; the connect service
	// reads it from the file (run.go), and embedders such as alt keep it off.
	EnableSidecarPush bool
	// Each publisher's (§6.6).
	SidecarPublisher pushservice.SidecarPublisherSettings
}

func DefaultExchangeSidecarPushSettings() *ExchangeSidecarPushSettings {
	return &ExchangeSidecarPushSettings{
		EnableSidecarPush: false,
		SidecarPublisher:  *pushservice.DefaultSidecarPublisherSettings(),
	}
}

// Whether this exchange hosts publishers now: the ceiling, MG0's table, and the switch as sidecar.yml
// reads now. Reads no state of its own beyond the file's cached read.
func (self *Exchange) sidecarPushOn() bool {
	return self.settings.EnableSidecarPush && server.SidecarMg0Linked() && server.CurrentSidecarConfig().Push()
}

// The profile a resident is built from and, while push is on or sidecar clients are counted
// (model.SidecarClientCountingOn, QUOTA-M1-04), the client's marks from the same read. A test's profile
// replaces the read whole, and its marks then come from residentPushMarksForTest.
func (self *Exchange) loadResidentProfile(ctx context.Context, clientId server.Id) (networkId server.Id, topLevel bool, category model.NetworkPeerCategory, peer *model.NetworkPeer, peersEnabled bool, marks model.SidecarPushMarks) {
	readMarks := self.sidecarPushOn() || model.SidecarClientCountingOn()
	if self.residentProfileForTest != nil {
		networkId, topLevel, category, peer, peersEnabled = self.residentProfileForTest(ctx, clientId)
		if readMarks && self.residentPushMarksForTest != nil {
			marks = self.residentPushMarksForTest(clientId)
		}
		return
	}
	if readMarks {
		return model.GetNetworkPeerProfileWithPushMarks(ctx, clientId)
	}
	networkId, topLevel, category, peer, peersEnabled = model.GetNetworkPeerProfile(ctx, clientId)
	return
}

// Starts the resident's publisher when its client is marked strict_exp and push is on, and answers how
// to stop it: the listener first, so no kick lands on a closing publisher, then Close, which waits for
// its run. Nothing to stop answers a no-op.
func (self *Resident) startSidecarPublisher() (stop func()) {
	stop = func() {}
	if !self.sidecarPushMarks.StrictExp || self.exchange.keyEventSubscriber == nil || self.exchange.sidecarPushRedis == nil {
		return
	}
	if !server.CurrentSidecarConfig().Push() {
		// the switch went off since the resident read its marks
		return
	}
	publisher, err := pushservice.NewSidecarPublisher(&self.exchange.settings.SidecarPublisher, nil)
	if err != nil {
		sidecarPublisherSettingsRefused.Inc()
		glog.Errorf("[r]sidecar publisher settings refused: %s\n", err)
		return
	}
	networkId := server.Id{}
	if self.sessionNetworkId != nil {
		networkId = *self.sessionNetworkId
	}
	// registered first, so an event between the reset's reads and the registration still kicks it
	removeListener := self.exchange.keyEventSubscriber.AddSidecarPushListener(self.clientId, publisher)
	publisher.Start(self.ctx, &pushservice.PublisherEnv{
		ClientId:  connect.Id(self.clientId),
		NetworkId: connect.Id(networkId),
		Send:      self.sendPublisherFrame,
		Redis:     self.exchange.sidecarPushRedis,
		// every topic: a sidecar's client
		Topics: nil,
	})
	sidecarPublishersStarted.Inc()
	if self.exchange.afterSidecarPublisherStartForTest != nil {
		self.exchange.afterSidecarPublisherStartForTest(self, publisher)
	}
	return func() {
		removeListener()
		publisher.Close()
	}
}

// Sends one publisher frame to the resident's client, beside sendListenerFrame, with connect's frame
// ack. Takes on success: a true return takes the frame's pool buffer, and on false the publisher still
// owns it and returns it. Blocks as the client's send does, until the client takes the frame or closes,
// which a resident's end does.
func (self *Resident) sendPublisherFrame(frame *protocol.Frame, ack connect.AckFunction) bool {
	return self.client.Send(frame, connect.Id(self.clientId), ack)
}
