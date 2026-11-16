package monitor

import (
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// §28.2's per-country half and DNSPod's trim. China's own set holds as many
// whole records as Alidns delivers, about three, and DNSPod, which cuts an
// answer it cannot carry at record boundaries (2026-11-12), hands its clients
// one whole record of it (owner, 2026-11-12: "let's do the best thing for
// users"). Through the default settings, so DNSPod is named by the endpoints
// the sdk gives cn. This file uses only what the half exposed before it judged
// DNSPod's trim, so it also runs against that code, where the first case
// fails.

// China's set of three records, Route 53's answer for every location here.
func extenderDnsTestChinaSet(t *testing.T) []string {
	return extenderDnsCountryTestValues(t, 300, 3, extenderDnsTestNow.Add(-4*time.Minute))
}

// A source that answers every resolver with China's whole set, but DNSPod's
// DoH servers, which answer as dnspod does, and Alidns's, as alidns does.
func extenderDnsTestTrimSource(t *testing.T, set []string, dnspod func() []byte, alidns func() []byte) *extenderDnsCountrySource {
	return &extenderDnsCountrySource{
		syntheticSource: &syntheticSource{},
		txt: func(string) (DNSResponseObservation, error) {
			return extenderDnsTestAnswer(set), nil
		},
		doh: func(dohUrl string, _ int) (int, []byte, error) {
			switch dohUrl {
			case "https://1.12.12.12/dns-query", "https://120.53.53.53/dns-query":
				return 200, dnspod(), nil
			case "https://223.5.5.5/dns-query", "https://223.6.6.6/dns-query":
				return 200, alidns(), nil
			}
			return 200, extenderDnsCountryDohBody(t, set, nil), nil
		},
		authoritative: func(netip.Prefix) (DNSResponseObservation, error) {
			return extenderDnsTestAnswer(set), nil
		},
	}
}

func TestExtenderDnsCountryTakesDnspodsTrimOfChinasSetAsWhole(t *testing.T) {
	const zone = "main/extender-dns-records"
	stale := func(path string) string { return "extender-dns-country-stale " + zone + "/" + path }
	partial := func(path string) string { return "extender-dns-country-partial " + zone + "/" + path }
	set := extenderDnsTestChinaSet(t)
	whole := func() []byte { return extenderDnsCountryDohBody(t, set, nil) }

	for _, test := range []struct {
		name   string
		dnspod func() []byte
		alidns func() []byte
		want   map[string]string
	}{
		{
			// one whole record of the three, unmarked: DNSPod's trim, which
			// is what its clients are meant to get
			name:   "one whole record",
			dnspod: func() []byte { return extenderDnsCountryDohBody(t, set[:1], nil) },
			alidns: whole,
			want: map[string]string{
				stale("cn/DoH 1.12.12.12"):     "healthy",
				partial("cn/DoH 1.12.12.12"):   "healthy",
				stale("cn/DoH 120.53.53.53"):   "healthy",
				partial("cn/DoH 120.53.53.53"): "healthy",
				partial("cn/DoH 223.5.5.5"):    "healthy",
			},
		},
		{
			// a value that is not a record beside a whole one is broken, not a
			// trim
			name: "a broken value",
			dnspod: func() []byte {
				return extenderDnsCountryDohBody(t, []string{set[0], "bm90IGEgcmVjb3Jk"}, nil)
			},
			alidns: whole,
			want: map[string]string{
				partial("cn/DoH 1.12.12.12"): "warn",
			},
		},
		{
			// marked truncated with none: no record at all
			name: "an empty answer",
			dnspod: func() []byte {
				return extenderDnsCountryDohBody(t, nil, func(message *dnsmessage.Message) { message.Header.Truncated = true })
			},
			alidns: whole,
			want: map[string]string{
				stale("cn/DoH 1.12.12.12"): "warn",
			},
		},
		{
			// Alidns delivers China's set whole, so a run of it from Alidns is
			// a partial set
			name:   "Alidns trimming",
			dnspod: func() []byte { return extenderDnsCountryDohBody(t, set[:1], nil) },
			alidns: func() []byte { return extenderDnsCountryDohBody(t, set[:1], nil) },
			want: map[string]string{
				partial("cn/DoH 223.5.5.5"):  "warn",
				partial("cn/DoH 1.12.12.12"): "healthy",
			},
		},
	} {
		states, _ := extenderDnsCountryStates(t, DefaultExtenderDnsRecordsSettings(), extenderDnsTestTrimSource(t, set, test.dnspod, test.alidns))
		for identity, want := range test.want {
			if states[identity] != want {
				t.Errorf("%s: %s is %q, want %q", test.name, identity, states[identity], want)
			}
		}
	}

	// China's set alone fits what DNSPod delivers whole, so DNSPod cutting it
	// is a partial set
	alone := set[:1]
	states, _ := extenderDnsCountryStates(t, DefaultExtenderDnsRecordsSettings(), extenderDnsTestTrimSource(t, alone,
		func() []byte { return extenderDnsCountryDohBody(t, nil, nil) },
		func() []byte { return extenderDnsCountryDohBody(t, alone, nil) },
	))
	if got := states[stale("cn/DoH 1.12.12.12")]; got != "warn" {
		t.Errorf("a one-record set DNSPod answers with none: %s is %q, want %q", stale("cn/DoH 1.12.12.12"), got, "warn")
	}
}
