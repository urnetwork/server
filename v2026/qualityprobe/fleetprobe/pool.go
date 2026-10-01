// This file is what a pass needs before it opens a tunnel: the destination
// pool, the operator endpoints derived from the api url, and the due list as
// providers with their places.
package fleetprobe

import (
	"context"
	"net/http"
	"strings"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

// Returns the destination pool the next probe uses. Like PinSource
// it is a snapshot getter: a long-lived command refreshes what it returns once
// per pass (see LoadPool), a task fetches once and hands every batch of the
// pass the same snapshot. A missing source has no destinations; it is a local
// configuration failure, never permission to use a different target catalog.
type PoolSource func() *egresshealth.Pool

// Resolves a source without manufacturing targets when configuration is absent.
func (self PoolSource) pool() *egresshealth.Pool {
	if self != nil {
		if pool := self(); pool != nil {
			return pool
		}
	}
	return &egresshealth.Pool{}
}

// Fetches the server's explicit destination catalog for one pass. A failed
// fetch returns no pool; callers report a local visibility failure and retry.
//
// client is a control-plane client (the pool comes from the operator's server
// directly, never through a provider), and poolUrl is normally PoolUrl of the
// api url.
func LoadPool(ctx context.Context, client *http.Client, poolUrl string, operatorSecret string) (*egresshealth.Pool, error) {
	pool, err := egresshealth.FetchPool(ctx, client, poolUrl, operatorSecret)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// Returns where the server serves the destination pool, given its api url.
func PoolUrl(apiUrl string) string {
	return strings.TrimRight(apiUrl, "/") + egresshealth.PoolPath
}

// Returns the operator's /ip echo URL for compatibility with older callers.
// Provider quality probes no longer fetch this URL as a warm-up or exit-IP
// check; they request only their randomized destination samples. Empty for an
// empty api URL.
func IpEchoUrl(apiUrl string) string {
	if strings.TrimSpace(apiUrl) == "" {
		return ""
	}
	return strings.TrimRight(apiUrl, "/") + egresshealth.IpEchoPath
}

// Turns a due list into the providers a pass probes, each
// with the place it is published under.
func ProvidersFromDue(due []ingest.DueProvider) []prober.Provider {
	providers := make([]prober.Provider, 0, len(due))
	for _, entry := range due {
		providers = append(providers, prober.Provider{
			ClientId:             entry.ClientId,
			RunsNeeded:           max(entry.RunsNeeded, entry.SuccessesNeeded),
			SuccessesNeeded:      entry.SuccessesNeeded,
			CycleStartedAt:       entry.CycleStartedAt,
			OutcomeCount:         entry.OutcomeCount,
			ClaimOrdinal:         entry.ClaimOrdinal,
			SecurityDestinations: entry.SecurityDestinations,
			Place: egresshealth.Place{
				Country: strings.ToLower(strings.TrimSpace(entry.CountryCode)),
				Region:  strings.TrimSpace(entry.Region),
			},
		})
	}
	return providers
}

// A batch with no places, for a caller that only has
// ids (the enumeration fallback, a manual probe).
func ProvidersFromClientIds(clientIds []string) []prober.Provider {
	providers := make([]prober.Provider, 0, len(clientIds))
	for _, clientId := range clientIds {
		providers = append(providers, prober.Provider{ClientId: clientId})
	}
	return providers
}

// A copy of a pool's profile for one probe's options, so no two
// probes share the Headers map through a pointer.
func profileOf(pool *egresshealth.Pool) *egresshealth.RequestProfile {
	profile := egresshealth.RequestProfile{UserAgent: pool.Profile.UserAgent}
	if pool.Profile.Headers != nil {
		profile.Headers = make(map[string]string, len(pool.Profile.Headers))
		for name, value := range pool.Profile.Headers {
			profile.Headers[name] = value
		}
	}
	return &profile
}

// Every host one probe's tunnel client may dial: the table's,
// the echo's, and any the caller adds (the bandwidth targets). It is the
// tunnel's allowlist, and the set a served pin must fall in to be used.
func dialHosts(tableHosts []string, echoUrl string, extra ...string) []string {
	seen := map[string]bool{}
	var hosts []string
	add := func(host string) {
		if host != "" && !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	for _, host := range tableHosts {
		add(host)
	}
	if echoUrl != "" {
		for _, host := range egresshealth.HostsOf([]egresshealth.Destination{{Url: echoUrl}}) {
			add(host)
		}
	}
	for _, host := range extra {
		add(host)
	}
	return hosts
}
