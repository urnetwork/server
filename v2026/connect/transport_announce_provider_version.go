package connect

// The provider rollout gauge and the bookkeeping that counts each transport of
// a publicly providing client under one bounded app release label.

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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

const (
	providerAppVersionUnknown = "unknown"
	providerAppVersionOther   = "other"

	// releases this recent keep their day, so a rollout can be followed
	// release by release while it happens; older releases fold into their
	// month. The window and the date checks bound the labels.
	providerAppVersionDayWindow = 62 * 24 * time.Hour
	// a release dated after now by more than this is not a real release
	providerAppVersionFutureSlack = 48 * time.Hour
	// app versions are calendar releases since 2025.3.31
	providerAppVersionMinYear = 2025
)

// Normalizes a client's app version (X-UR-AppVersion, or the auth message's
// app_version) to a bounded label. The apps send their calendar release with an
// optional build code: "2026.10.1-1060587890" (Android, Windows) or "2026.10.1"
// (Apple). The build code is dropped. A release within
// providerAppVersionDayWindow of now keeps its day, "2026.10.1"; an older one
// is its month, "2025.12". No version is unknown; anything else, including a
// date in the future, is other, so the label set stays bounded whatever a
// client sends.
func providerAppVersionLabel(appVersion string, now time.Time) string {
	appVersion = strings.TrimSpace(appVersion)
	if appVersion == "" {
		return providerAppVersionUnknown
	}
	appVersion = strings.TrimPrefix(appVersion, "v")
	if i := strings.IndexAny(appVersion, "-+ _"); 0 <= i {
		appVersion = appVersion[:i]
	}
	parts := strings.Split(appVersion, ".")
	if len(parts) != 3 {
		return providerAppVersionOther
	}
	var numbers [3]int
	for i, part := range parts {
		if part == "" || 4 < len(part) || strings.Trim(part, "0123456789") != "" {
			return providerAppVersionOther
		}
		numbers[i], _ = strconv.Atoi(part)
	}
	year, month, day := numbers[0], numbers[1], numbers[2]
	release := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if year < providerAppVersionMinYear ||
		release.Year() != year || release.Month() != time.Month(month) || release.Day() != day ||
		now.Add(providerAppVersionFutureSlack).Before(release) {
		return providerAppVersionOther
	}
	if now.Sub(release) <= providerAppVersionDayWindow {
		return fmt.Sprintf("%d.%d.%d", year, month, day)
	}
	return fmt.Sprintf("%d.%d", year, month)
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
