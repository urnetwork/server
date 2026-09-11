package session

// Privacy-preserving API authentication aggregates.

import (
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/jwt"
)

var sessionAuthAttemptsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "session",
	Name:      "auth_attempts_total",
	Help:      "Request authentication attempts by bounded credential kind and validation outcome.",
}, []string{"method", "outcome"})

// sessionActiveAuthCollector retains only one-way principal digests for the
// rolling lifetime needed by process-local active-network aggregates.
type sessionActiveAuthCollector struct {
	description *prometheus.Desc
	stateLock   sync.Mutex
	lastSeen    map[[sha256.Size]byte]time.Time
	now         func() time.Time
}

// newSessionActiveAuthCollector constructs an empty active-principal set.
func newSessionActiveAuthCollector() *sessionActiveAuthCollector {
	return &sessionActiveAuthCollector{
		description: prometheus.NewDesc(
			"urnetwork_session_active_authenticated_networks",
			"Distinct authenticated network principals seen by this process in the rolling window; sums across API replicas may duplicate networks.",
			[]string{"window"}, nil,
		),
		lastSeen: map[[sha256.Size]byte]time.Time{},
		now:      time.Now,
	}
}

var sessionActiveAuth = newSessionActiveAuthCollector()

func init() {
	for _, method := range []string{"none", "jwt", "api_key", "other"} {
		for _, outcome := range []string{"succeeded", "rejected"} {
			sessionAuthAttemptsTotal.WithLabelValues(method, outcome)
		}
	}
	prometheus.MustRegister(sessionAuthAttemptsTotal, sessionActiveAuth)
}

// Describe implements prometheus.Collector.
func (self *sessionActiveAuthCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.description
}

// Collect implements prometheus.Collector and never emits retained digests.
func (self *sessionActiveAuthCollector) Collect(metrics chan<- prometheus.Metric) {
	now := self.now()
	active := map[string]int{"5m": 0, "1h": 0, "24h": 0}
	self.stateLock.Lock()
	for principal, seenAt := range self.lastSeen {
		age := now.Sub(seenAt)
		if age > 24*time.Hour {
			delete(self.lastSeen, principal)
			continue
		}
		if age <= 5*time.Minute {
			active["5m"]++
		}
		if age <= time.Hour {
			active["1h"]++
		}
		active["24h"]++
	}
	self.stateLock.Unlock()
	for _, window := range []string{"5m", "1h", "24h"} {
		metrics <- prometheus.MustNewConstMetric(self.description, prometheus.GaugeValue, float64(active[window]), window)
	}
}

// observe stores a digest of a successfully authenticated network principal.
func (self *sessionActiveAuthCollector) observe(byJwt *jwt.ByJwt) {
	if byJwt == nil {
		return
	}
	digest := sha256.Sum256([]byte(byJwt.NetworkId.String()))
	self.stateLock.Lock()
	self.lastSeen[digest] = self.now()
	self.stateLock.Unlock()
}

// requestAuthMethod maps the credential shape to a finite method without
// retaining or exporting any credential bytes.
func requestAuthMethod(req *http.Request) string {
	authorization := req.Header.Get("Authorization")
	if authorization == "" {
		return "none"
	}
	if !strings.HasPrefix(authorization, authBearerPrefix) {
		return "other"
	}
	if strings.HasPrefix(authorization[len(authBearerPrefix):], "urn_") {
		return "api_key"
	}
	return "jwt"
}

// recordSessionAuth records validation outcome and successful principal
// activity without route, identity, credential, or error labels.
func recordSessionAuth(method string, byJwt *jwt.ByJwt, err error) {
	outcome := "rejected"
	if err == nil && byJwt != nil {
		outcome = "succeeded"
		sessionActiveAuth.observe(byJwt)
	}
	sessionAuthAttemptsTotal.WithLabelValues(method, outcome).Inc()
}
