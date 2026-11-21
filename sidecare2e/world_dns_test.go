//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The world's DNS helpers (world_test.go): a network's ACL document saved by its owner's device, a record written
// through dns's api by a device's own dns client, and the world's dns edge asked over UDP until it answers what a
// write put, as a resolver asks an NS.

package sidecare2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/sidecart3"
)

// What the DNS helpers wait with (QD-702).
type WorldDnsSettings struct {
	// How often a write the dns API answers unavailable, its gate not yet holding the network's newest state, is
	// tried again, and how often the edge is asked again for an answer it does not give yet: the API's cache follows
	// api's {acl_}v within §8.9's 5 s and the edge's journal hold wakes at each serial raise, with no hook either end.
	RetryInterval time.Duration
	// The bound on each such wait.
	Timeout time.Duration
	// How long one UDP exchange with the edge may take.
	ExchangeTimeout time.Duration
	// The UDP buffer a query advertises with DO set (EDNS0), so a signed answer fits.
	UdpBufferByteCount uint16
}

func DefaultWorldDnsSettings() *WorldDnsSettings {
	return &WorldDnsSettings{
		RetryInterval:      100 * time.Millisecond,
		Timeout:            60 * time.Second,
		ExchangeTimeout:    5 * time.Second,
		UdpBufferByteCount: 4096,
	}
}

// device, the network's config writer, saves source as the network's ACL document (sidecart3's
// T3Network.SaveAclPolicy), synced at the base's pin at every gate the world's syncs wait on. A base api refuses 429,
// past §7.9's config_bases_per_min the world's limits hold (one a minute), is planned and saved again once its
// retry_after has passed, as a person runs `ur acl push` again, within WorldSettings.RateLimitWaitTimeout.
func (self *World) SaveAclPolicy(ctx context.Context, network *WorldNetwork, device *sidecart3.T3Device, source string) (*bridge.AuthPin, error) {
	ctx, cancel := context.WithTimeout(ctx, self.settings.RateLimitWaitTimeout)
	defer cancel()
	for {
		pin, err := network.SaveAclPolicy(ctx, device, []byte(source))
		var apiErr *sidecar.SidecarApiError
		if err == nil || !errors.As(err, &apiErr) || apiErr.Status != http.StatusTooManyRequests {
			return pin, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w, and its retry_after outlasted the wait: %w", err, ctx.Err())
		case <-time.After(time.Duration(apiErr.RetryAfterMs) * time.Millisecond):
		}
	}
}

// device's dns client writes upsert through dns's api, trying again while the API answers unavailable: its gate's
// cache follows the log on its own, so a write right after a statement may find it a version behind. Any other
// refusal is the answer.
func (self *World) SetDnsRecord(ctx context.Context, device *sidecart3.T3Device, upsert *sidecar.DnsRecordUpsert, settings *WorldDnsSettings) (*sidecar.DnsRecordsAnswer, error) {
	if settings == nil {
		settings = DefaultWorldDnsSettings()
	}
	client, err := device.Sidecar().Dns()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		answer, err := client.SetRecord(ctx, upsert, nil)
		var apiErr *sidecar.DnsApiError
		if err == nil || !errors.As(err, &apiErr) || apiErr.Code != sidecar.DnsApiCodeUnavailable {
			return answer, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the dns API stayed unavailable to %s: %w (%w)", device.Name, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// One question to the world's dns edge over UDP, with DO set so the answer carries its signatures.
func (self *World) DnsQuery(ctx context.Context, name string, questionType uint16, settings *WorldDnsSettings) (*dns.Msg, error) {
	if settings == nil {
		settings = DefaultWorldDnsSettings()
	}
	edge := self.Block(WorldBlockDnsedge)
	if edge == nil {
		return nil, errors.New("the world runs no dns edge")
	}
	address := edge.UdpAddress()
	if address == "" {
		return nil, errors.New("the dns edge is stopped")
	}
	question := new(dns.Msg)
	question.SetQuestion(dns.Fqdn(name), questionType)
	question.SetEdns0(settings.UdpBufferByteCount, true)
	client := &dns.Client{Net: "udp", Timeout: settings.ExchangeTimeout}
	answer, _, err := client.ExchangeContext(ctx, question, address)
	return answer, err
}

// Asks the world's dns edge for name's questionType until an answer want takes, and returns it: the edge serves a
// write once its journal poll brings the row in and its view checks, with no hook, so it is asked every
// RetryInterval within Timeout. The last answer and error otherwise.
func (self *World) AwaitDnsAnswer(ctx context.Context, name string, questionType uint16, want func(answer *dns.Msg) bool, settings *WorldDnsSettings) (*dns.Msg, error) {
	if settings == nil {
		settings = DefaultWorldDnsSettings()
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Timeout)
	defer cancel()
	for {
		answer, err := self.DnsQuery(ctx, name, questionType, settings)
		if err == nil && want(answer) {
			return answer, nil
		}
		select {
		case <-ctx.Done():
			return answer, fmt.Errorf("the dns edge never answered %s %s as wanted: %v (%w)", name, dns.TypeToString[questionType], err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// Whether answer holds exactly one TXT record of name's, whose text is text, signed (an RRSIG over the TXT set beside
// it).
func dnsAnswerHoldsSignedTxt(answer *dns.Msg, name string, text string) bool {
	if answer == nil || answer.Rcode != dns.RcodeSuccess {
		return false
	}
	held, other, signed := 0, 0, false
	for _, record := range answer.Answer {
		switch typed := record.(type) {
		case *dns.TXT:
			if dns.CanonicalName(typed.Hdr.Name) == dns.CanonicalName(name) && strings.Join(typed.Txt, "") == text {
				held += 1
			} else {
				other += 1
			}
		case *dns.RRSIG:
			signed = signed || typed.TypeCovered == dns.TypeTXT
		}
	}
	return held == 1 && other == 0 && signed
}
