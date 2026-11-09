package sidecarhost

// ServiceHost.Gates on server's reader of config/<env>/sidecar.yml (host_gates.go; §19.11, D-OPS-10):
// the root's ServiceGates over server.CurrentSidecarConfig, which keeps p0-auth's failure rules: an
// absent file is inert, a broken first read opens nothing, and a later broken read keeps the last good
// one.
//
// No method does I/O. Gates holds the latest read as an immutable snapshot, which its goroutine renews
// from the reader every PollInterval until its ctx ends; the reader itself rereads the file once its
// read is 60 s old, so a gate moves within a minute and a poll of a config push. Safe for concurrent use.

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

var _ sidecar.ServiceGates = (*Gates)(nil)

// The services Gates answers by name; any other answers closed.
var gatesServiceNames = []string{
	sidecar.ServiceGateEvs,
	sidecar.ServiceGateGkm,
	sidecar.ServiceGateSockets,
	sidecar.ServiceGateTlsedge,
	sidecar.ServiceGateMxedge,
	sidecar.ServiceGateSmtpedge,
	sidecar.ServiceGateDns,
}

// The tunables.
type GatesSettings struct {
	// How often the snapshot is renewed from server's reader: 5 s. The reader rereads the file only once
	// its read is §19.11's 60 s old, so this bounds the lag behind a reread, not the reads.
	PollInterval time.Duration
	// What a refused caller is asked to wait: §19.11's reread interval, 60 s.
	RetryAfter time.Duration
}

// The defaults.
func DefaultGatesSettings() *GatesSettings {
	return &GatesSettings{
		PollInterval: 5 * time.Second,
		RetryAfter:   60 * time.Second,
	}
}

// One read of the file, as the root's types.
type gatesSnapshot struct {
	config   *server.SidecarConfig
	services map[string]sidecar.ServiceGate
	format2  sidecar.Format2Gate
}

// A read as the root's types: each service's gate, and format 2's.
func newGatesSnapshot(config *server.SidecarConfig) *gatesSnapshot {
	adapt := func(gate *server.SidecarServiceGate) sidecar.ServiceGate {
		networkIds := []connect.Id{}
		for _, networkId := range gate.NetworkIds() {
			networkIds = append(networkIds, connect.Id(networkId))
		}
		// Writes is the gate's writes inside an enabled gate, and a closed gate takes none either way
		return sidecar.NewServiceGate(gate.Enabled(), gate.Writes(), networkIds, gate.Percent())
	}
	snapshot := &gatesSnapshot{
		config:   config,
		services: map[string]sidecar.ServiceGate{},
	}
	for _, name := range gatesServiceNames {
		snapshot.services[name] = adapt(config.Service(name))
	}
	format2 := config.Format2()
	snapshot.format2 = sidecar.NewFormat2Gate(adapt(format2.Gate()), string(format2.Purge()), string(format2.Drop()))
	return snapshot
}

// The gates. NewGates makes one.
type Gates struct {
	settings *GatesSettings
	read     func() *server.SidecarConfig
	latest   atomic.Pointer[gatesSnapshot]
}

// The process's gates, read once now and renewed until ctx ends, with settings, nil being the
// defaults.
func NewGates(ctx context.Context, settings *GatesSettings) *Gates {
	return newGates(ctx, settings, server.CurrentSidecarConfig)
}

func newGates(ctx context.Context, settings *GatesSettings, read func() *server.SidecarConfig) *Gates {
	if settings == nil {
		settings = DefaultGatesSettings()
	}
	gates := &Gates{
		settings: settings,
		read:     read,
	}
	gates.renew()
	go server.HandleError(func() {
		gates.run(ctx)
	})
	return gates
}

// Takes the reader's latest read as the snapshot.
func (self *Gates) renew() {
	config := self.read()
	if current := self.latest.Load(); current != nil && current.config == config {
		return
	}
	self.latest.Store(newGatesSnapshot(config))
}

func (self *Gates) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(self.settings.PollInterval):
		}
		self.renew()
	}
}

// See sidecar.ServiceGates.
func (self *Gates) Service(service string) sidecar.ServiceGate {
	return self.latest.Load().services[service]
}

// See sidecar.ServiceGates.
func (self *Gates) Format2() sidecar.Format2Gate {
	return self.latest.Load().format2
}

// See sidecar.ServiceGates.
func (self *Gates) Switch(name string) bool {
	return self.latest.Load().config.Switch(name)
}

// See sidecar.ServiceGates.
func (self *Gates) PlaceNew() string {
	return self.latest.Load().config.PlaceNew()
}

// See sidecar.ServiceGates.
func (self *Gates) Reseed() bool {
	return self.latest.Load().config.Reseed()
}

// See sidecar.ServiceGates.
func (self *Gates) Drained() []string {
	return self.latest.Load().config.Drained()
}

// See sidecar.ServiceGates.
func (self *Gates) SocketsControlQps() (float64, int, bool) {
	return self.latest.Load().config.SocketsControlQps()
}

// See sidecar.ServiceGates.
func (self *Gates) UrnetworkEdge() (connect.Id, bool) {
	networkId, ok := self.latest.Load().config.UrnetworkEdge()
	if !ok {
		return connect.Id{}, false
	}
	return connect.Id(networkId), true
}

// See sidecar.ServiceGates: an absent entry has passed nothing.
func (self *Gates) FileCluster(clusterId string) sidecar.FileClusterGate {
	cluster, ok := self.latest.Load().config.FileCluster(clusterId)
	if !ok {
		return sidecar.FileClusterGate{}
	}
	return sidecar.FileClusterGate{
		SettlePassedBuild: cluster.SettlePassedBuild,
		HddCapacityOps:    cluster.HddCapacityOps,
	}
}

// See sidecar.ServiceGates.
func (self *Gates) RetryAfter() time.Duration {
	return self.settings.RetryAfter
}
