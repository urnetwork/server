// URL policy is explicit catalog data, not a destination class exception.
package egresshealth

import "fmt"

// This binary implements this exact outcome contract, not future versions.
const UrlProbePolicyVersion = 1

// Versioned independently of the catalog contents so old success contracts
// cannot accidentally acquire the meaning of the URL-only workflow.
type UrlProbePolicy struct {
	Version            int   `json:"version" yaml:"version"`
	MaxRedirects       int   `json:"max_redirects" yaml:"max_redirects"`
	MaxBodyBytes       int   `json:"max_body_bytes" yaml:"max_body_bytes"`
	MaxTtfbMillis      int   `json:"max_ttfb_ms" yaml:"max_ttfb_ms"`
	MinThroughputBps   int64 `json:"min_throughput_bps" yaml:"min_throughput_bps"`
	MinThroughputBytes int   `json:"min_throughput_bytes" yaml:"min_throughput_bytes"`
}

// Bounds remain configurable. The throughput sample floor and TTFB boundary
// are intentionally distinct from the legacy destination status contracts.
func DefaultUrlProbePolicy() UrlProbePolicy {
	return UrlProbePolicy{Version: UrlProbePolicyVersion, MaxRedirects: 5, MaxBodyBytes: 1024 * 1024, MaxTtfbMillis: 2000, MinThroughputBps: 100000, MinThroughputBytes: 16 * 1024}
}

// A malformed policy is a local catalog error, never a provider outcome.
func (self UrlProbePolicy) Validate() error {
	if self.Version != UrlProbePolicyVersion {
		return fmt.Errorf("URL probe policy version %d is unsupported", self.Version)
	}
	if self.MaxRedirects < 0 || self.MaxRedirects > 5 {
		return fmt.Errorf("URL probe max_redirects must be between 0 and 5")
	}
	if self.MaxBodyBytes < 1 || self.MaxBodyBytes > 1024*1024 {
		return fmt.Errorf("URL probe max_body_bytes must be between 1 and 1048576")
	}
	if self.MaxTtfbMillis <= 0 || self.MinThroughputBps <= 0 || self.MinThroughputBytes < 1 || self.MinThroughputBytes > self.MaxBodyBytes {
		return fmt.Errorf("URL probe timing limits must be positive and the sample floor cannot exceed the read cap")
	}
	return nil
}

func (self Options) urlProbePolicy() UrlProbePolicy {
	if self.UrlProbePolicy != nil {
		return *self.UrlProbePolicy
	}
	return DefaultUrlProbePolicy()
}
