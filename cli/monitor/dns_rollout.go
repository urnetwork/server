package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/urnetwork/sidecar/dnsservice"

	servermonitor "github.com/urnetwork/server/monitor"
	"github.com/urnetwork/server/router"
)

// The version and status must come from the same private response as the signed RRset proof.
type dnsRolloutCheck struct {
	private   *http.Client
	statusUrl string
	version   string
	settings  *servermonitor.DnsCanarySettings
}

// Every configured authority must agree over both transports. Each ns1 exchange
// also needs the exact ready process to recognize its fresh signed denial. After
// the journal/Unbound canary, bind the validated TXT on each path to that process too.
func checkDnsEdgeServing(ctx context.Context, settings *servermonitor.DnsCanarySettings, statusUrl, expected string, checkTxt bool) error {
	if settings == nil || settings.ConfigError != "" || settings.Dns == nil || settings.Timeout <= 0 || settings.Fresh <= 0 ||
		settings.RolloutStatusMaxBytes <= 0 || len(settings.Dns.Roots) == 0 || len(settings.Dns.Ns) < 2 || !slices.Contains(settings.Dns.Ns, "ns1") {
		return errors.New("DNS rollout needs the configured roots and the public addresses of ns1 and its peers")
	}
	check := &dnsRolloutCheck{
		private: &http.Client{
			Timeout:       settings.Timeout,
			Transport:     &http.Transport{DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		statusUrl: statusUrl, version: expected, settings: settings,
	}
	for _, root := range settings.Dns.Roots {
		var firstSerial *uint32
		for _, ns := range settings.Dns.Ns {
			addresses := settings.NsAddresses[ns]
			if len(addresses) == 0 {
				return fmt.Errorf("%s has no public DNS address", ns)
			}
			seen := map[netip.AddrPort]bool{}
			for _, address := range addresses {
				if seen[address] {
					continue
				}
				seen[address] = true
				for _, transport := range []string{"tcp", "udp"} {
					client := &dns.Client{Net: transport, Timeout: settings.Timeout}
					serial, err := check.read(ctx, client, address, root, ns == "ns1", checkTxt)
					if err != nil {
						return fmt.Errorf("%s %s %s: %w", ns, root, transport, err)
					}
					if firstSerial == nil {
						firstSerial = &serial
					} else if serial != *firstSerial {
						return fmt.Errorf("%s %s %s: SOA serials differ", ns, root, transport)
					}
				}
			}
		}
	}
	return nil
}

// Require an answer from the specified transport. UDP truncation may never fall back to TCP here.
func dnsRolloutExchange(ctx context.Context, client *dns.Client, address netip.AddrPort, name string, rrType uint16, signed bool) (*dns.Msg, error) {
	query := new(dns.Msg)
	query.SetQuestion(name, rrType)
	query.RecursionDesired = false
	if signed {
		query.SetEdns0(dns.DefaultMsgSize, true)
	}
	reply, _, err := client.ExchangeContext(ctx, query, address.String())
	if err != nil {
		return nil, err
	}
	if !reply.Response || !reply.Authoritative || reply.Truncated || reply.Rcode != dns.RcodeSuccess ||
		len(reply.Question) != 1 || reply.Question[0] != query.Question[0] {
		return nil, errors.New("no complete authoritative answer to the question")
	}
	return reply, nil
}

// A new question for each endpoint prevents one route or a previous release check from
// supplying another route's proof. The challenge's signed denial also carries the SOA.
func (self *dnsRolloutCheck) read(ctx context.Context, client *dns.Client, address netip.AddrPort, root string, first bool, checkTxt bool) (uint32, error) {
	var nonce dnsservice.DnsRolloutNonce
	owner, rrType := root, uint16(dns.TypeSOA)
	if first {
		rand.Read(nonce[:])
		owner, rrType = nonce.Owner(root), dns.TypeA
	}
	reply, err := dnsRolloutExchange(ctx, client, address, owner, rrType, first)
	if err != nil {
		return 0, err
	}
	records := reply.Answer
	if first {
		if len(reply.Answer) != 0 {
			return 0, errors.New("the rollout challenge was not denied")
		}
		records = reply.Ns
	}
	var soa *dns.SOA
	for _, record := range records {
		if rr, ok := record.(*dns.SOA); ok {
			if soa != nil || rr.Hdr.Name != root || rr.Hdr.Class != dns.ClassINET {
				return 0, errors.New("unexpected SOA")
			}
			soa = rr
		}
	}
	if soa == nil {
		return 0, errors.New("no authoritative SOA")
	}
	if !first {
		return soa.Serial, nil
	}
	if _, err := self.confirm(ctx, root, hex.EncodeToString(nonce[:]), owner, dns.TypeNSEC, records, soa.Serial); err != nil {
		return 0, err
	}
	if checkTxt {
		canary := dnsservice.DnsCanaryOwner(root)
		reply, err := dnsRolloutExchange(ctx, client, address, canary, dns.TypeTXT, true)
		if err != nil {
			return 0, err
		}
		if len(reply.Answer) != 2 {
			return 0, errors.New("the canary response must contain its TXT and signature")
		}
		record, err := self.confirm(ctx, root, dnsservice.DnsCanaryLabel, canary, dns.TypeTXT, reply.Answer, soa.Serial)
		if err != nil {
			return 0, err
		}
		written, err := dnsservice.ParseDnsCanaryValue(strings.Join(record.(*dns.TXT).Txt, ""))
		if err != nil || time.Since(time.UnixMilli(written)) >= self.settings.Fresh {
			return 0, errors.New("the confirmed TXT canary is not fresh")
		}
	}
	return soa.Serial, nil
}

// The private request never asks the target to sign. It can only confirm its own
// cached, verified signature on precisely the RRset received from the public socket.
func (self *dnsRolloutCheck) confirm(ctx context.Context, root, challenge, owner string, rrType uint16, records []dns.RR, serial uint32) (dns.RR, error) {
	var data dns.RR
	var signature *dns.RRSIG
	for _, record := range records {
		header := record.Header()
		if header.Name != owner || header.Class != dns.ClassINET {
			continue
		}
		if header.Rrtype == rrType {
			if data != nil {
				return nil, errors.New("multiple records for the rollout question")
			}
			data = record
		}
		if rr, ok := record.(*dns.RRSIG); ok && rr.TypeCovered == rrType && rr.SignerName == root {
			if signature != nil {
				return nil, errors.New("multiple signatures for the rollout question")
			}
			signature = rr
		}
	}
	proof := dnsservice.DnsRolloutRecordProof(data, signature)
	if proof == "" {
		return nil, errors.New("no signed RRset for the rollout question")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, self.statusUrl, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Cache-Control", "no-cache, no-store")
	request.Header.Set(dnsservice.DnsRolloutProofHeader, challenge+" "+root)
	response, err := self.private.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	want := strconv.FormatUint(uint64(serial), 10) + " " + proof
	if response.StatusCode != http.StatusOK || response.Header.Get(dnsservice.DnsRolloutProofHeader) != want {
		return nil, errors.New("the private block did not confirm this public DNS response")
	}
	var status router.WarpStatusResult
	if err := json.NewDecoder(io.LimitReader(response.Body, self.settings.RolloutStatusMaxBytes)).Decode(&status); err != nil {
		return nil, err
	}
	if status.Service != "dnsedge-1" || status.Block != "g1" || status.Version == nil || *status.Version != self.version || status.Status != "ok" {
		return nil, fmt.Errorf("dnsedge-1/g1 is not ready at %s", self.version)
	}
	return data, nil
}
