// This file is what a pass needs before it opens a tunnel: the destination
// pool, the operator endpoints derived from the api url, and the due list as
// providers with their places.
package fleetprobe

import (
	"context"
	"net/http"
	"strings"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/prober"
)

// Returns the destination pool the next probe uses. Like PinSource
// it is a snapshot getter: a long-lived command refreshes what it returns once
// per pass (see LoadPool), a task fetches once and hands every batch of the
// pass the same snapshot. A nil source, or a nil pool, means the built-in
// table.
type PoolSource func() *egresshealth.Pool

// Resolves a source, falling back to the built-in table.
func (self PoolSource) pool() *egresshealth.Pool {
	if self != nil {
		if pool := self(); pool != nil {
			return pool
		}
	}
	return egresshealth.BuiltinPool()
}

// Fetches the server's destination pool for one pass. It always
// returns a pool it is safe to run: the server's, or -- when the fetch fails
// for any reason, which the error then says -- the built-in table, which is
// the seed the server's pool starts from and always a well-defined
// measurement. A pass never stops for want of a pool; the error is for the
// log and the metric.
//
// client is a control-plane client (the pool comes from the operator's server
// directly, never through a provider), and poolUrl is normally PoolUrl of the
// api url.
func LoadPool(ctx context.Context, client *http.Client, poolUrl string, operatorSecret string) (*egresshealth.Pool, error) {
	pool, err := egresshealth.FetchPool(ctx, client, poolUrl, operatorSecret)
	if err != nil {
		return egresshealth.BuiltinPool(), err
	}
	return pool, nil
}

// Returns where the server serves the destination pool, given its api url.
func PoolUrl(apiUrl string) string {
	return strings.TrimRight(apiUrl, "/") + egresshealth.PoolPath
}

// Returns the operator's /ip echo on apiUrl, which every run and check
// fetches first, through the provider's tunnel, for its warm-up and its exit
// address. The api url has to be the address the api answers on from the
// public internet, since the request leaves from the provider. Empty for an
// empty api url.
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
			ClientId: entry.ClientId,
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
