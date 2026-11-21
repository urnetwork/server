package dnshost

// dnsservice.DnsCanaryWriter over model.DnsWrite (DNS-M5-17,
// plan-DNS-canary-route-state): the site monitor's canary.<root> TXT through
// §12.6's one write path, a root's own record of kind canary, which expires an
// hour after its last write like a challenge (dnsCommitInTx) and which
// taskworker's chain removes through the journal. Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/model"
)

var (
	_ dnsservice.DnsCanaryWriter       = (*Canaries)(nil)
	_ dnsservice.DnsCanaryVerdictStore = (*Canaries)(nil)
	_ dnsservice.DnsReadyStore         = (*Canaries)(nil)
)

// The writer. It holds nothing.
type Canaries struct{ RestoreStore }

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

// The same stored verdicts drive the monitoring endpoint and the certs readiness check.
func (self *Canaries) PutCanaryVerdict(ctx context.Context, root string, verdict dnsservice.DnsCanaryVerdict) error {
	return storeCall(func() error { return model.PutDnsCanaryVerdict(ctx, root, verdict) })
}

func (self *Canaries) CanaryVerdicts(ctx context.Context, root string) (verdicts []dnsservice.DnsCanaryVerdict, returnErr error) {
	returnErr = storeCall(func() error {
		var err error
		verdicts, err = model.GetDnsCanaryVerdicts(ctx, root)
		return err
	})
	return
}

// Reconcile dns.yml before declaring the storage dependencies ready.
func (self *Canaries) ProbeReadiness(ctx context.Context, root string, names []string) error {
	return storeCall(func() error {
		if err := model.SyncDnsNameServers(ctx, root, names); err != nil {
			return err
		}
		_, err := model.GetDnsCanaryVerdicts(ctx, root)
		return err
	})
}
