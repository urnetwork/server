package connect

// The provider rollout gauge and the bookkeeping that counts each transport of
// a publicly providing client under one bounded app release label.

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
)

// The provider rollout gauge: this process's connections from clients that
// provide publicly, by app release. A provider-side change, such as a new
// connect security policy, reaches users only as providers update, and this
// is where that rollout can be read.
var providerConnectionsGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "provider_connections",
		Help:      "Connections from clients that provide publicly, by app release (year.month.day for recent releases, year.month for older ones, unknown when none was sent, other when unrecognized)",
	},
	[]string{"app_version"},
)

// Registers the gauge with the default registry.
func init() {
	prometheus.MustRegister(providerConnectionsGauge)
}

// Normalizes a client's app version (X-UR-AppVersion, or the auth message's
// app_version) to a bounded label (server.AppVersionLabel).
func providerAppVersionLabel(appVersion string, now time.Time) string {
	return server.AppVersionLabel(appVersion, now)
}

// Counts connections per label and deletes a label's series when its count
// returns to zero, so only labels with a live provider connection are exported.
// The gauge is set under stateLock, so a series always carries the count of the
// last change and a removal cannot delete a series another connection just set;
// the gauge itself takes only its own short in-memory lock.
type providerVersionConnections struct {
	gauge *prometheus.GaugeVec

	stateLock   sync.Mutex
	labelCounts map[string]int
}

// Counts that export into gauge, starting at zero.
func newProviderVersionConnections(gauge *prometheus.GaugeVec) *providerVersionConnections {
	return &providerVersionConnections{
		gauge:       gauge,
		labelCounts: map[string]int{},
	}
}

var defaultProviderVersionConnections = newProviderVersionConnections(providerConnectionsGauge)

// Counts one more connection under label.
func (self *providerVersionConnections) add(label string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.labelCounts[label] += 1
	self.gauge.WithLabelValues(label).Set(float64(self.labelCounts[label]))
}

// Counts one connection less under label, deleting its series at zero.
func (self *providerVersionConnections) remove(label string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	count := self.labelCounts[label] - 1
	if count <= 0 {
		delete(self.labelCounts, label)
		self.gauge.DeleteLabelValues(label)
		return
	}
	self.labelCounts[label] = count
	self.gauge.WithLabelValues(label).Set(float64(count))
}

// One transport's place in the gauge. Only the announce run goroutine touches
// it.
type providerVersionConnection struct {
	connections *providerVersionConnections
	appVersion  string
	// the label the connection is counted under, "" while it is not counted
	label string
}

// A connection of a client with appVersion, not counted until its first update.
func newProviderVersionConnection(connections *providerVersionConnections, appVersion string) *providerVersionConnection {
	return &providerVersionConnection{
		connections: connections,
		appVersion:  appVersion,
	}
}

// Counts the connection under its current label while its client provides
// publicly, and not otherwise. The label is recomputed each time, so a long
// connection moves from its release day to its month.
func (self *providerVersionConnection) update(providing bool, now time.Time) {
	label := ""
	if providing {
		label = providerAppVersionLabel(self.appVersion, now)
	}
	if label == self.label {
		return
	}
	if self.label != "" {
		self.connections.remove(self.label)
	}
	if label != "" {
		self.connections.add(label)
	}
	self.label = label
}

// Removes the connection from the gauge when it ends.
func (self *providerVersionConnection) release() {
	if self.label != "" {
		self.connections.remove(self.label)
		self.label = ""
	}
}
