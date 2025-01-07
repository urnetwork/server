package dot

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sync/errgroup"
)

// DNS cache struct with a map and a mutex for concurrency safety
type DNSCache struct {
	mu              sync.RWMutex
	cache           map[string]cachedResult
	LookupsPeformed *atomic.Uint64
}

type cachedResult struct {
	addresses []string
	expiresAt time.Time
	err       error
}

// const cacheTTL = 5 * time.Minute // Time-to-live for cache entries

// NewDNSCache creates and initializes a DNS cache
func NewDNSCache() *DNSCache {
	return &DNSCache{
		cache:           make(map[string]cachedResult),
		LookupsPeformed: new(atomic.Uint64),
	}
}

// Resolve performs DNS lookup over TLS with caching
func (c *DNSCache) Resolve(ctx context.Context, domain string) ([]string, error) {
	c.mu.RLock()
	if result, found := c.cache[domain]; found {
		// Check if the cache entry is still valid
		if time.Since(result.expiresAt) < 0 {
			c.mu.RUnlock()
			return result.addresses, result.err
		}
	}
	c.mu.RUnlock()

	eg, ctx := errgroup.WithContext(ctx)

	var v4Adresses []string
	var v4Expiration time.Time
	var v6Adresses []string
	var v6Expiration time.Time

	eg.Go(func() (err error) {
		v4Adresses, v4Expiration, err = resolveDoT(ctx, domain, dns.TypeA)
		if err != nil {
			return fmt.Errorf("failed to resolve A addresses for %s: %w", domain, err)
		}

		return nil
	})

	eg.Go(func() (err error) {
		v6Adresses, v6Expiration, err = resolveDoT(ctx, domain, dns.TypeAAAA)
		if err != nil {
			return fmt.Errorf("failed to resolve AAAA addresses for %s: %w", domain, err)
		}

		return nil
	})

	err := eg.Wait()
	if err != nil {
		c.mu.Lock()
		c.cache[domain] = cachedResult{
			addresses: nil,
			expiresAt: time.Now().Add(30 * time.Second),
			err:       err,
		}
		c.mu.Unlock()
		return nil, err
	}

	earliestExpiration := v4Expiration
	if v6Expiration.Before(earliestExpiration) {
		earliestExpiration = v6Expiration
	}

	allAddresses := append(v4Adresses, v6Adresses...)

	// Cache the result
	c.mu.Lock()
	c.cache[domain] = cachedResult{
		addresses: append(v4Adresses, v6Adresses...),
		expiresAt: earliestExpiration,
	}
	c.mu.Unlock()

	c.LookupsPeformed.Add(1)

	return allAddresses, nil
}

var client = &dns.Client{
	Net: "tcp-tls",
}

func exchange(ctx context.Context, m *dns.Msg, address string) (*dns.Msg, time.Duration, error) {
	co, err := client.DialContext(ctx, address)
	if err != nil {
		return nil,
			0,
			err
	}

	defer co.Close()
	return client.ExchangeWithConnContext(ctx, m, co)
}

// resolveDoT resolves DNS over TLS for a domain
func resolveDoT(ctx context.Context, domain string, recordType uint16) ([]string, time.Time, error) {

	// TLS resolver server (e.g., Cloudflare's DoT server)
	serverAddr := "9.9.9.9:853" // Cloudflare DNS over TLS

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(domain), recordType)

	// Perform the DNS query over TLS
	resp, _, err := exchange(ctx, msg, serverAddr)
	if err != nil {
		return nil,
			time.Now(),
			err
	}

	if resp.Rcode != dns.RcodeSuccess {
		return nil, time.Now(), fmt.Errorf("failed to resolve domain: %s", domain)
	}

	// Collect IP addresses from the response
	var addresses []string
	earliestExpiration := time.Now().Add(time.Hour)
	for _, ans := range resp.Answer {
		if aRecord, ok := ans.(*dns.A); ok {
			// ans.Header().Ttl = 300 // Set the TTL to 300 seconds
			exp := time.Now().Add(time.Duration(aRecord.Header().Ttl) * time.Second)
			if exp.Before(earliestExpiration) {
				earliestExpiration = exp
			}
			addresses = append(addresses, aRecord.A.String())
		}
	}

	return addresses, earliestExpiration, nil
}
