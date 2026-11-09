package dnshost

// dnsservice.DnsCanaryWriter over model.DnsWrite (DNS-M5-17,
// plan-DNS-canary-route-state): the site monitor's canary.<root> TXT through
// §12.6's one write path, a root's own record of kind canary, which expires an
// hour after its last write like a challenge (dnsCommitInTx) and which
// taskworker's chain removes through the journal. Safe for concurrent use.
//
// dns_canary_verdict waits for its §2.8 row (plan-DNS-metadata-rows #7, v9),
// so the API block keeps no verdict: it is given no DnsCanaryVerdictStore,
// answers every report 503, and GET ready answers canary_failing, the
// resolution's interim.

import (
	"context"
	"errors"
	"fmt"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/model"
)

var _ dnsservice.DnsCanaryWriter = (*Canaries)(nil)

// The writer. It holds nothing.
type Canaries struct{}

func NewCanaries() *Canaries {
	return &Canaries{}
}

// Writes value at canary.<root>: a value the route would refuse, or one the
// path refuses, is dnsservice.ErrDnsCanaryRefused, and a restore's refusal
// passes as it is.
func (self *Canaries) PutCanary(ctx context.Context, root string, value string, ttl uint32) (serial uint64, returnErr error) {
	if _, err := dnsservice.ParseDnsCanaryValue(value); err != nil {
		return 0, err
	}
	root = dns.CanonicalName(root)
	returnErr = storeCall(func() error {
		result, err := model.DnsWrite(ctx, &model.DnsWriteArgs{
			Root: root,
			Upserts: []dnsservice.DnsRecordSet{{
				Owner: dnsservice.DnsCanaryOwner(root),
				Type:  dns.TypeTXT,
				Ttl:   ttl,
				Kind:  dnsservice.DnsRecordKindCanary,
				Rdata: []string{dnsservice.DnsCanaryRdata(value)},
			}},
		})
		switch {
		case err == nil:
			serial = result.Serial
			return nil
		case errors.Is(err, model.ErrDnsOwnerOutsideZone), errors.Is(err, dnsservice.ErrDnsZoneLint):
			return fmt.Errorf("%w: %w", dnsservice.ErrDnsCanaryRefused, err)
		}
		return err
	})
	return
}
