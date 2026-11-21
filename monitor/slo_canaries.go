package monitor

import (
	"context"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/sidecar/dnsservice"
)

// Both paths terminate at the same published TLS origin and verify the same server name.
type SloWarmTlsSettings struct {
	ServerName string `yaml:"server_name"`
	Edge       string `yaml:"edge"`
	Direct     string `yaml:"direct"`
}

func (self *SloWarmTlsSettings) valid() bool {
	edge, edgeErr := netip.ParseAddrPort(self.Edge)
	direct, directErr := netip.ParseAddrPort(self.Direct)
	return validDNSDomain(self.ServerName) && edgeErr == nil && directErr == nil && edge.Port() != 0 && direct.Port() != 0 && edge != direct
}

func (self *SloWarmTlsSettings) measure(ctx context.Context, handshake func(context.Context, string, string, string) (TLSCertificateObservation, error), now func() time.Time) (time.Duration, bool) {
	if !self.valid() {
		return 0, false
	}
	run := func(address string) (time.Duration, bool) {
		start := now()
		peer, err := handshake(ctx, "tcp", address, self.ServerName)
		return now().Sub(start), err == nil && peer.VerifyError == nil && len(peer.Certificates) > 0
	}
	// Warm both routes, including the routed listener, before the measured pair.
	if _, ok := run(self.Direct); !ok {
		return 0, false
	}
	if _, ok := run(self.Edge); !ok {
		return 0, false
	}
	direct, ok := run(self.Direct)
	if !ok {
		return 0, false
	}
	edge, ok := run(self.Edge)
	return max(0, edge-direct), ok
}

type sloCanaryMetrics struct {
	warmTls prometheus.Histogram
	dns     *prometheus.CounterVec
}

var defaultSloCanaryMetrics = sync.OnceValue(func() *sloCanaryMetrics {
	metrics := &sloCanaryMetrics{
		warmTls: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "urnetwork_monitor_tlsedge_warm_setup_overhead_seconds",
			Help:    "Warm verified TLS handshake through the edge minus the same origin's direct handshake.",
			Buckets: []float64{0.3},
		}),
		dns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "urnetwork_monitor_dns_propagation_total",
			Help: "Journal canary writes observed at both authoritative servers by the propagation deadline.",
		}, []string{"result"}),
	}
	prometheus.MustRegister(metrics.warmTls, metrics.dns)
	return metrics
})

// Query exactly this write through the authoritative server's configured addresses. The regular canary
// separately checks DNSSEC and permits its previous fresh value for its health verdict.
func (self *dnsCanaryDirectNs) servesCanary(ctx context.Context, root string, addresses []netip.AddrPort, value string) bool {
	query := new(dns.Msg)
	query.SetQuestion(dnsservice.DnsCanaryOwner(root), dns.TypeTXT)
	query.RecursionDesired = false
	reply, err := self.exchange(ctx, addresses, query)
	return err == nil && authoritativeCanaryPresent(reply, query.Question[0].Name, value)
}

func authoritativeCanaryPresent(reply *dns.Msg, owner string, value string) bool {
	if reply == nil || !reply.Authoritative {
		return false
	}
	for _, record := range reply.Answer {
		if txt, ok := record.(*dns.TXT); ok && strings.EqualFold(txt.Hdr.Name, owner) && strings.Join(txt.Txt, "") == value {
			return true
		}
	}
	return false
}

// A read at each authority must complete by the deadline. A canceled monitor run yields no measurement.
func observeDnsPropagation(ctx context.Context, started time.Time, settings *DnsCanarySettings, root, value string, serves func(context.Context, string, []netip.AddrPort, string) bool) string {
	if len(settings.Dns.Ns) < 2 || settings.PropagationTarget <= 0 || settings.PropagationPoll <= 0 {
		return ""
	}
	deadline := started.Add(settings.PropagationTarget)
	probeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	seen := make(map[string]bool, len(settings.Dns.Ns))
	for probeCtx.Err() == nil {
		for _, ns := range settings.Dns.Ns {
			if !seen[ns] && serves(probeCtx, root, settings.NsAddresses[ns], value) {
				seen[ns] = true
			}
		}
		if len(seen) == len(settings.Dns.Ns) && !time.Now().After(deadline) && ctx.Err() == nil {
			return "on_time"
		}
		select {
		case <-probeCtx.Done():
		case <-time.After(settings.PropagationPoll):
		}
	}
	if ctx.Err() != nil {
		return ""
	}
	return "late"
}
