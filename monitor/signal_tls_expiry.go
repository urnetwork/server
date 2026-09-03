package monitor

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// SIGNALS.md §18.2 maps to signal_tls_expiry.go and
// signal_tls_expiry_test.go. The probe presents the configured manager SNI to
// every exact public LB address so DNS health selection cannot hide one stale
// certificate generation.
func NewTLSExpirySignal() Signal {
	return &signalAdapter{
		number: "18.2",
		key:    "tls-expiry",
		name:   "Public TLS certificate expiry",
		probe:  tlsExpiryProbe{},
	}
}

type tlsExpiryProbe struct{}

func (tlsExpiryProbe) id() string             { return "synthetic/tls-expiry" }
func (tlsExpiryProbe) tier() string           { return tierPage }
func (tlsExpiryProbe) cadence() time.Duration { return 5 * time.Minute }

const tlsExpiryWarningWindow = 21 * 24 * time.Hour

type tlsExpiryTarget struct {
	host          string
	interfaceName string
	family        string
	network       string
	address       string
}

func (t tlsExpiryTarget) identityTarget() string {
	return t.host + "/" + t.interfaceName
}

func (t tlsExpiryTarget) dialAddress() string {
	return net.JoinHostPort(t.address, "443")
}

type tlsExpiryResult struct {
	target      tlsExpiryTarget
	observation TLSCertificateObservation
	leaf        *x509.Certificate
	err         error
}

func (tlsExpiryProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hostname := strings.TrimSpace(env.cfg.managerHostname)
	if hostname == "" {
		return nil, nil
	}
	targets := tlsExpiryTargets(env)
	if len(targets) == 0 {
		return nil, fmt.Errorf("manager TLS is configured for %s but no enabled public LB interface is available", hostname)
	}

	results := make(chan tlsExpiryResult, len(targets))
	semaphore := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for _, queued := range targets {
		target := queued
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- tlsExpiryResult{target: target, err: ctx.Err()}
				return
			}

			result := tlsExpiryResult{target: target}
			result.observation, result.err = env.runner.tlsCertificates(
				ctx, target.network, target.dialAddress(), hostname,
			)
			if result.err == nil {
				if len(result.observation.Certificates) == 0 {
					result.err = fmt.Errorf("peer returned no certificate")
				} else {
					result.leaf, result.err = x509.ParseCertificate(result.observation.Certificates[0])
				}
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]tlsExpiryResult, 0, len(targets))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].target.host != ordered[j].target.host {
			return ordered[i].target.host < ordered[j].target.host
		}
		if ordered[i].target.interfaceName != ordered[j].target.interfaceName {
			return ordered[i].target.interfaceName < ordered[j].target.interfaceName
		}
		return ordered[i].target.family < ordered[j].target.family
	})

	findings := make([]finding, 0, len(ordered))
	for _, result := range ordered {
		findings = append(findings, tlsExpiryFinding(hostname, env.now(), result))
	}
	return findings, nil
}

func tlsExpiryTargets(env *probeEnv) []tlsExpiryTarget {
	targets := []tlsExpiryTarget{}
	for _, configuredHost := range env.cfg.hosts {
		for _, configured := range configuredHost.publicLB {
			if address := strings.TrimSpace(configured.IPv4Address); address != "" {
				targets = append(targets, tlsExpiryTarget{
					host: configuredHost.name, interfaceName: configured.Interface,
					family: "ipv4", network: "tcp4", address: address,
				})
			}
			if address := strings.TrimSpace(configured.IPv6Address); address != "" {
				targets = append(targets, tlsExpiryTarget{
					host: configuredHost.name, interfaceName: configured.Interface,
					family: "ipv6", network: "tcp6", address: address,
				})
			}
		}
	}
	return targets
}

func tlsExpiryFinding(hostname string, now time.Time, result tlsExpiryResult) finding {
	target := result.target.identityTarget()
	base := finding{
		probeId: "synthetic/tls-expiry", tier: tierPage,
		target: target, frame: result.target.family, sustain: 1,
		baseline: fmt.Sprintf(
			"Every exact public LB address serves a system-trusted leaf covering %s that is currently valid with more than %s remaining.",
			hostname, tlsExpiryWarningWindow,
		),
		verify: fmt.Sprintf(
			"After the final LB handoff, require three consecutive five-minute exact-address observations where %s is covered, system verification succeeds, and more than %s remains.",
			hostname, tlsExpiryWarningWindow,
		),
		playbook: "SIGNALS.md §18.2",
	}
	endpoint := fmt.Sprintf("%s/%s %s=%s", result.target.host, result.target.interfaceName, result.target.family, result.target.address)
	if result.err != nil {
		base.tier = tierWarn
		base.class = "tls-certificate-unobservable"
		base.sustain = 2
		base.symptom = fmt.Sprintf("The monitor could not read %s's certificate from %s", hostname, endpoint)
		base.mechanism = "The bounded direct TLS handshake failed or returned no parseable leaf. Certificate validity on this exact edge is unknown; a healthy DNS-selected sibling cannot close it."
		base.observed = fmt.Sprintf("endpoint=%s error=%s", endpoint, result.err)
		base.action = "Correlate this endpoint with edge-ipv6 and exact ingress. Restore the observation path first; do not infer certificate health, change DNS, or replace certificate material from a transport failure alone."
		return base
	}

	leaf := result.leaf
	evidence := tlsCertificateEvidence(hostname, endpoint, leaf, result.observation.VerifyError)
	remaining := leaf.NotAfter.Sub(now)
	base.observed = fmt.Sprintf(
		"endpoint=%s not_before=%s not_after=%s remaining=%s",
		endpoint,
		leaf.NotBefore.UTC().Format(time.RFC3339),
		leaf.NotAfter.UTC().Format(time.RFC3339),
		formatCertificateDuration(remaining),
	)
	base.evidence = evidence
	base.context = "The handshake intentionally captures an invalid leaf before applying ordinary system-root verification; it never treats bypassed verification as client success."
	base.action = tlsCertificateRepairAction(hostname)

	if now.Before(leaf.NotBefore) {
		base.class = "tls-certificate-not-yet-valid"
		base.symptom = fmt.Sprintf("%s serves a certificate that is not valid yet on %s", hostname, endpoint)
		base.mechanism = "The exact LB selected a leaf whose validity interval starts in the future. Clock skew or prematurely promoted certificate material makes ordinary clients reject the handshake."
		return base
	}
	if !now.Before(leaf.NotAfter) {
		base.class = "tls-certificate-expired"
		base.symptom = fmt.Sprintf("%s serves an expired certificate on %s", hostname, endpoint)
		base.mechanism = "The exact LB selected a leaf past NotAfter. This commonly occurs when a newly exposed alias was absent from the renewed SAN certificate and Warp fell back to an older wildcard asset, or when a corrected Vault snapshot has not crossed the LB drain boundary."
		return base
	}
	if err := leaf.VerifyHostname(hostname); err != nil {
		base.class = "tls-certificate-hostname"
		base.symptom = fmt.Sprintf("%s is not covered by the certificate served on %s", hostname, endpoint)
		base.mechanism = "The exact LB selected a currently dated certificate whose SAN set does not cover the requested SNI. Adding DNS or a service alias without issuing and promoting matching certificate material creates this failure."
		base.observed += " hostname_verification=" + err.Error()
		return base
	}
	if result.observation.VerifyError != nil {
		base.class = "tls-certificate-untrusted"
		base.symptom = fmt.Sprintf("%s serves a certificate chain that system roots reject on %s", hostname, endpoint)
		base.mechanism = "The leaf is in its validity window and covers the hostname, but ordinary system-root verification rejects the presented chain. A missing/wrong intermediate or untrusted issuer is distinct from expiry and routing."
		base.observed += " system_verification=" + result.observation.VerifyError.Error()
		return base
	}
	if remaining <= tlsExpiryWarningWindow {
		base.tier = tierWarn
		base.class = "tls-certificate-expiring"
		base.symptom = fmt.Sprintf("%s's certificate has only %s remaining on %s", hostname, formatCertificateDuration(remaining), endpoint)
		base.mechanism = "The certificate is usable now but has entered the renewal safety window. Waiting until NotAfter turns this into a client-visible outage, especially when LB drains delay certificate activation."
		return base
	}

	base.class = "tls-certificate-valid"
	base.healthy = true
	return base
}

func tlsCertificateRepairAction(hostname string) string {
	return fmt.Sprintf(
		"Inspect the newest promoted Vault TLS directory for an exact %s asset whose SAN and dates are valid. If it is absent, an authorized operator must run `warpctl certs issue <env>`, review/promote the generated `tls.pending` version, and deploy Vault/LB configuration through the existing edge workflow. If it is already present, let the controlled LB drain finish and compare the replacement container's selected certificate path before considering a targeted retry. Do not bypass client verification, change DNS, or restart unrelated services.",
		hostname,
	)
}

func tlsCertificateEvidence(hostname, endpoint string, leaf *x509.Certificate, verifyErr error) string {
	fingerprint := sha256.Sum256(leaf.Raw)
	verification := "ok"
	if verifyErr != nil {
		verification = verifyErr.Error()
	}
	hostnameCovered := leaf.VerifyHostname(hostname) == nil
	return fmt.Sprintf(
		"endpoint=%s subject=%q issuer=%q serial=%s sha256=%x dns_name_count=%d hostname_covered=%t system_verification=%s",
		endpoint,
		leaf.Subject.String(),
		leaf.Issuer.String(),
		leaf.SerialNumber.String(),
		fingerprint,
		len(leaf.DNSNames),
		hostnameCovered,
		verification,
	)
}

func formatCertificateDuration(duration time.Duration) string {
	if duration < 0 {
		return "-" + formatCertificateDuration(-duration)
	}
	return (duration / time.Hour * time.Hour).String()
}
