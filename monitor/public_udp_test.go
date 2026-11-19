package monitor

// The public UDP enrollment matrix (public_udp.go) for the targets LBALT adds:
// connect's lb_alt front serves the api names over h3 and whodis (LBALT.md
// D3), so a target with service connect and front api is a valid row.

import (
	"reflect"
	"testing"
)

func publicUdpTestConnectApiSettings() PublicUdpSettings {
	return PublicUdpSettings{
		Enabled:         true,
		ExpectedTargets: 1,
		Targets: []PublicUdpTargetSettings{{
			Name:        "synthetic-connect-api",
			Host:        "edge-synthetic",
			Interface:   "eth1",
			Service:     "connect",
			Alias:       "whodis-53",
			Front:       "api",
			Carrier:     "dns",
			Families:    []string{"ipv4", "ipv6"},
			IPv4Address: "192.0.2.20",
			IPv6Address: "2001:db8::20",
			Port:        53,
			ServerName:  "api.synthetic.example",
			DnsTld:      "codec.synthetic.example.",
		}},
	}
}

// A connect api row expands into one request per family with its protocol
// authority intact; before LBALT the whole matrix was refused as
// protocol-authority.
func TestPublicUdpConnectApiTargetIsAccepted(t *testing.T) {
	hosts := []*host{{name: "edge-synthetic"}}
	requests, reason := publicUdpRequests(publicUdpTestConnectApiSettings(), hosts)
	if reason != "" {
		t.Fatalf("a connect api target was refused: %s", reason)
	}
	want := []PublicUdpRequest{
		{
			Host: "edge-synthetic", Target: "synthetic-connect-api", Family: "ipv4", Service: "connect", Front: "api", Carrier: "dns",
			Address: "192.0.2.20", Port: 53, ServerName: "api.synthetic.example", DnsTld: "codec.synthetic.example.",
		},
		{
			Host: "edge-synthetic", Target: "synthetic-connect-api", Family: "ipv6", Service: "connect", Front: "api", Carrier: "dns",
			Address: "2001:db8::20", Port: 53, ServerName: "api.synthetic.example", DnsTld: "codec.synthetic.example.",
		},
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %+v, want %+v", requests, want)
	}
}

// Every pair of service and front that a front serves is a valid row, and the
// protocol authority still refuses any other service or front.
func TestPublicUdpProtocolAuthorityAcceptsOnlyServedFronts(t *testing.T) {
	hosts := []*host{{name: "edge-synthetic"}}
	cases := []struct {
		service string
		front   string
		reason  string
	}{
		{service: "connect", front: "connect", reason: ""},
		{service: "connect", front: "api", reason: ""},
		{service: "alt", front: "connect", reason: ""},
		{service: "alt", front: "api", reason: ""},
		{service: "api", front: "api", reason: "protocol-authority"},
		{service: "connect", front: "web", reason: "protocol-authority"},
	}
	for _, c := range cases {
		settings := publicUdpTestConnectApiSettings()
		settings.Targets[0].Service, settings.Targets[0].Front = c.service, c.front
		requests, reason := publicUdpRequests(settings, hosts)
		if reason != c.reason || (reason == "") != (len(requests) == 2) {
			t.Errorf("service %s front %s: reason %q with %d requests, want %q", c.service, c.front, reason, len(requests), c.reason)
		}
	}
}
