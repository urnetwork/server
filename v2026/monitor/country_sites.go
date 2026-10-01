package monitor

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// CountrySiteCandidate is source evidence for a proposed country-specific
// website, not a scored destination. Verification and publication happen
// separately; rank 0 means the source offers only a rank bucket.
type CountrySiteCandidate struct {
	URL    string
	Source string
	Rank   int
}

// CanonicalSiteKey identifies one website across www/subdomain and URL-path
// variants. It rejects non-HTTPS URLs, credentials, IP literals and invalid
// hostnames so an untrusted ranking response cannot introduce a raw endpoint.
// Public Suffix List ownership matters here: example.co.uk must not collapse
// to co.uk, while www.example.co.uk and shop.example.co.uk must collide.
func CanonicalSiteKey(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil || u.Host == "" {
		return "", fmt.Errorf("country site must be an HTTPS URL with a hostname")
	}
	if u.Port() != "" {
		return "", fmt.Errorf("country site must not override the HTTPS port")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || net.ParseIP(host) != nil || strings.ContainsAny(host, ":%") {
		return "", fmt.Errorf("country site has an invalid hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("country site has an invalid hostname")
		}
		for _, c := range label {
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return "", fmt.Errorf("country site has an invalid hostname")
			}
		}
	}
	key, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return "", fmt.Errorf("country site has no registrable domain: %w", err)
	}
	return key, nil
}

// FilterCountrySiteCandidates removes every country candidate whose
// registrable domain is already in the shared global list, and removes
// duplicates within the country. It preserves source order and does not pad
// a short list. Invalid candidate URLs are counted as rejected, not silently
// accepted. A malformed global URL is a configuration error: otherwise a
// failed global parse could let an overlap through.
func FilterCountrySiteCandidates(globalURLs []string, country []CountrySiteCandidate) (accepted []CountrySiteCandidate, rejected int, err error) {
	seen := make(map[string]bool, len(globalURLs)+len(country))
	for _, rawURL := range globalURLs {
		key, keyErr := CanonicalSiteKey(rawURL)
		if keyErr != nil {
			return nil, 0, fmt.Errorf("global site: %w", keyErr)
		}
		seen[key] = true
	}
	for _, candidate := range country {
		key, keyErr := CanonicalSiteKey(candidate.URL)
		if keyErr != nil || seen[key] {
			rejected++
			continue
		}
		seen[key] = true
		accepted = append(accepted, candidate)
	}
	return accepted, rejected, nil
}

// MergeCountrySiteCandidates gives verified Radar candidates first choice,
// then fills from verified CrUX origins. Both sources are checked against the
// entire global list and each other before applying target. The caller must
// verify website loadability and source provenance before calling; this
// function does not turn a DNS ranking into a scored destination.
func MergeCountrySiteCandidates(globalURLs []string, radar []CountrySiteCandidate, crux []CountrySiteCandidate, target int) ([]CountrySiteCandidate, int, error) {
	if target <= 0 {
		return nil, 0, fmt.Errorf("country site target must be positive")
	}
	all := make([]CountrySiteCandidate, 0, len(radar)+len(crux))
	all = append(all, radar...)
	all = append(all, crux...)
	accepted, rejected, err := FilterCountrySiteCandidates(globalURLs, all)
	if err != nil {
		return nil, 0, err
	}
	if target < len(accepted) {
		accepted = accepted[:target]
	}
	return accepted, rejected, nil
}
