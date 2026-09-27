package egresshealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The server's destination pool: its wire format, fetching it, and refusing
// one that cannot be run.

// Where the server serves the active destination pool (GEOMAP
// §11.4): GET <api url>/network/provider-egress-destinations, under the
// operator secret the other egress routes use.
const PoolPath = "/network/provider-egress-destinations"

// The header every operator endpoint authenticates
// with. It is spelled out here rather than imported because ingest imports
// this package; TestFetchPoolSendsTheIngestOperatorSecretHeader (in ingest)
// holds the two spellings together.
const operatorSecretHeader = "X-UR-Operator-Secret"

// Bounds the pool response read. A pool is a few hundred
// destinations of a few hundred bytes each; the bound is what keeps a
// misbehaving server from filling the prober's memory with one answer.
const maxPoolBytes = 4 * 1024 * 1024

// The destination pool as the server serves it: the active sites per
// class with their load contracts, and the request profile to load them with.
// The server keeps it representative from the prober's own results (retiring
// sites that fail healthy exits, promoting candidates), which is why it is
// data rather than the compiled-in table; the table stays as the seed the
// server's pool starts from and as the fallback when it cannot be fetched.
type Pool struct {
	// Identifies the pool's contents, so a log line or a stored run
	// can say which pool it was drawn from. BuiltinPool is version 0.
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at"`
	// Sampled per run exactly as the built-in table is (see
	// sampleSizes); the blackhole check draws from its connectivity class.
	Destinations []Destination `json:"destinations"`
	// What every request carries. An empty UserAgent means the
	// default (see RequestProfile.orDefault).
	Profile RequestProfile `json:"profile"`
}

// Reports that the server did not answer the pool endpoint
// with a pool that can be run. Every caller's response to it is the same: run
// the built-in table instead (see fleetprobe.LoadPool). It is never a reason
// to stop probing, because the built-in table is always a correct, if less
// curated, measurement.
var ErrPoolUnavailable = errors.New("egresshealth: could not get a usable destination pool from the server")

// The compiled-in table as a pool: the seed the server's pool
// starts from and the fallback when it cannot be fetched.
func BuiltinPool() *Pool {
	return &Pool{
		Version:      0,
		Destinations: Destinations(),
		Profile:      DefaultRequestProfile(),
	}
}

// Fetches the server's active destination pool from url with a GET,
// authenticated with the operator secret exactly as the ingest submissions
// are, and returns it only if every destination in it can be run (see
// ValidateDestinations).
//
// A pool with one broken entry is refused whole rather than run without it.
// Running a partial pool would change the sample's composition -- a class
// short, a name missing from every run -- without anything saying so, while
// the fallback the caller takes instead, the built-in table, is always a
// well-defined measurement. The error names every entry that failed, so the
// server-side fault is one log line away.
func FetchPool(ctx context.Context, client *http.Client, url string, secret string) (*Pool, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: %w", ErrPoolUnavailable, ErrNilClient)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPoolUnavailable, err)
	}
	req.Header.Set(operatorSecretHeader, secret)

	resp, err := client.Do(req)
	if err != nil {
		// %w so a caller triaging a shutdown can still see context.Canceled.
		return nil, fmt.Errorf("%w: %w", ErrPoolUnavailable, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: status 401: the server rejected the operator secret", ErrPoolUnavailable)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: status 404: this server has not deployed %s", ErrPoolUnavailable, PoolPath)
	default:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: status %d: %s", ErrPoolUnavailable, resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var pool Pool
	// Unknown fields are allowed on purpose: the server keeps per-site
	// bookkeeping (category, region, retirement) the prober has no use for,
	// and may add more without a prober release.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxPoolBytes)).Decode(&pool); err != nil {
		return nil, fmt.Errorf("%w: decoding the response: %w", ErrPoolUnavailable, err)
	}
	if err := ValidateDestinations(pool.Destinations); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPoolUnavailable, err)
	}
	return &pool, nil
}

// Reports why a destination list cannot be run as a
// pool, or nil. It holds a pool to the rules the built-in table's own tests
// hold it to, so a server-side mistake is refused at the prober rather than
// measured as a provider fault on every exit:
//
//   - every destination is valid on its own (see Destination.Validate);
//   - names are unique, because they are what failed_names and the server's
//     per-site failure share key on;
//   - every scored class has at least one destination, because a class absent
//     from the pool is a fault mode nobody is watching, and the blackhole
//     check has nothing to draw without connectivity.
//
// The server can use it to vet a candidate before promoting it.
func ValidateDestinations(dests []Destination) error {
	if len(dests) == 0 {
		return ErrNoDestinations
	}
	var problems []string
	seen := map[string]bool{}
	perClass := map[Class]int{}
	for i, d := range dests {
		if err := d.Validate(); err != nil {
			problems = append(problems, fmt.Sprintf("destination %d (%q): %s", i, d.Name, err))
			continue
		}
		if seen[d.Name] {
			problems = append(problems, fmt.Sprintf("destination %d repeats the name %q", i, d.Name))
			continue
		}
		seen[d.Name] = true
		perClass[d.Class]++
	}
	for _, c := range Classes {
		if perClass[c] == 0 {
			problems = append(problems, fmt.Sprintf("class %q has no destination", c))
		}
	}
	if 0 < len(problems) {
		return errors.New("the pool cannot be run: " + strings.Join(problems, "; "))
	}
	return nil
}

// Reports why one destination cannot be loaded as declared, or nil.
//
// Every rule here is one the built-in table is already held to by its tests,
// restated for data that arrives over the network:
//
//   - a name, and a class that is one of Classes;
//   - an https url on the default port. Plaintext could be forged by the very
//     provider under test, and the tunnel refuses it anyway; another port
//     would fall outside the confinement self-check, which dials 443;
//   - a coherent success contract: ExpectStatus names a 2xx or 3xx (a 4xx is
//     a refusal and must stay a failure), ExpectBody and ExpectReachable name
//     none (it would be ignored), and ExpectReachable -- the weakest contract
//     -- stays confined to the site class;
//   - a body check it can run, and one where a status line and some bytes are
//     not evidence: every dns destination verifies its answer, and every
//     connectivity destination that reads a body verifies it, because those
//     are the endpoints a captive portal imitates;
//   - no negative byte cap (zero means MaxBodyBytes, and anything above the
//     ceiling is clamped to it, never honoured);
//   - incompatible places the prober can match: a lower-case alpha-2 country,
//     with or without a region. A place it could not match would silently
//     charge that place's exits with a site known not to work there.
func (self Destination) Validate() error {
	if strings.TrimSpace(self.Name) == "" {
		return errors.New("no name")
	}
	known := false
	for _, c := range Classes {
		if self.Class == c {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("class %q is not one of %v", self.Class, Classes)
	}

	u, err := url.Parse(self.Url)
	if err != nil {
		return fmt.Errorf("url %q does not parse: %w", self.Url, err)
	}
	if u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("url %q is not an https url with a host", self.Url)
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("url %q is on port %s; only 443 is covered by the confinement self-check", self.Url, port)
	}

	switch self.Expect {
	case ExpectBody:
		if self.Status != 0 {
			return fmt.Errorf("expect %q declares status %d, which it would ignore", self.Expect, self.Status)
		}
	case ExpectStatus:
		if self.Status < 200 || 400 <= self.Status {
			return fmt.Errorf("expect %q must name a 2xx or 3xx status (got %d)", self.Expect, self.Status)
		}
	case ExpectReachable:
		if self.Status != 0 {
			return fmt.Errorf("expect %q declares status %d, which it would ignore", self.Expect, self.Status)
		}
		if self.Class != ClassSite {
			return fmt.Errorf("expect %q is confined to class %q", self.Expect, ClassSite)
		}
	default:
		return fmt.Errorf("unknown expect %d", self.Expect)
	}

	if err := self.Verify.valid(); err != nil {
		return err
	}
	if self.Class == ClassDns && self.Verify.Kind != BodyCheckDnsJson {
		return fmt.Errorf("a dns destination must verify its answer with %q; a portal answers 200 with a body too", BodyCheckDnsJson)
	}
	if self.Class == ClassConnectivity && self.Expect == ExpectBody && self.Verify.Kind == "" {
		return errors.New("a connectivity destination that reads a body must verify it; these are the endpoints a captive portal imitates")
	}
	if self.MaxBytes < 0 {
		return fmt.Errorf("negative max_bytes %d", self.MaxBytes)
	}
	for _, place := range self.Incompatible {
		if err := place.valid(); err != nil {
			return err
		}
	}
	return nil
}
