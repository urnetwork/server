package monitor

// The public UDP enrollment matrix (public_udp.go) for the targets LBALT adds:
// connect's lb_alt front serves the api names over h3 and whodis (LBALT.md
// D3), so a target with service connect and front api is a valid row.

import (
	"fmt"
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
	requests, reason := publicUdpRequests(publicUdpTestConnectApiSettings(), DefaultPublicUdpProbeSettings(), hosts)
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
		requests, reason := publicUdpRequests(settings, DefaultPublicUdpProbeSettings(), hosts)
		if reason != c.reason || (reason == "") != (len(requests) == 2) {
			t.Errorf("service %s front %s: reason %q with %d requests, want %q", c.service, c.front, reason, len(requests), c.reason)
		}
	}
}

// Whodis serves one alias on 53 and on 4053, so two rows that differ only in
// their port are two paths, while an exact repeat is still refused.
func TestPublicUdpWhodisRowsOnBothPortsAreDistinctPaths(t *testing.T) {
	hosts := []*host{{name: "edge-synthetic"}}
	settings := publicUdpTestConnectApiSettings()
	port4053 := settings.Targets[0]
	port4053.Name = "synthetic-connect-api-4053"
	port4053.Port = 4053
	settings.Targets = append(settings.Targets, port4053)
	settings.ExpectedTargets = 2
	requests, reason := publicUdpRequests(settings, DefaultPublicUdpProbeSettings(), hosts)
	if reason != "" || len(requests) != 4 {
		t.Fatalf("whodis rows on 53 and 4053 gave %q with %d requests, want both rows' families", reason, len(requests))
	}

	settings.Targets[1].Port = 53
	if requests, reason := publicUdpRequests(settings, DefaultPublicUdpProbeSettings(), hosts); reason != "duplicate-path" || requests != nil {
		t.Fatalf("a repeated row gave %q with %d requests, want duplicate-path", reason, len(requests))
	}
}

// The lb_alt matrix, every carrier of both fronts on each of the ten lb
// interfaces, quic on 443 and dns on 53 and 4053 under one whodis alias, is
// 60 dual-stack rows, which the default bound admits.
func TestPublicUdpLbAltMatrixFitsTheDefaultBound(t *testing.T) {
	hosts := []*host{{name: "edge-synthetic"}}
	settings := PublicUdpSettings{Enabled: true}
	for index := range 10 {
		for _, front := range []string{"connect", "api"} {
			for _, carrier := range []struct {
				carrier string
				alias   string
				port    int
				dnsTld  string
			}{
				{carrier: "quic", alias: "primary", port: 443},
				{carrier: "dns", alias: "whodis", port: 53, dnsTld: "codec.synthetic.example."},
				{carrier: "dns", alias: "whodis", port: 4053, dnsTld: "codec.synthetic.example."},
			} {
				settings.Targets = append(settings.Targets, PublicUdpTargetSettings{
					Name:        fmt.Sprintf("synthetic-eth%d-%s-%d", index, front, carrier.port),
					Host:        "edge-synthetic",
					Interface:   fmt.Sprintf("eth%d", index),
					Service:     "connect",
					Alias:       carrier.alias,
					Front:       front,
					Carrier:     carrier.carrier,
					Families:    []string{"ipv4", "ipv6"},
					IPv4Address: fmt.Sprintf("192.0.2.%d", 10+index),
					IPv6Address: fmt.Sprintf("2001:db8::%d", 10+index),
					Port:        carrier.port,
					ServerName:  front + ".synthetic.example",
					DnsTld:      carrier.dnsTld,
				})
			}
		}
	}
	settings.ExpectedTargets = len(settings.Targets)
	requests, reason := publicUdpRequests(settings, DefaultPublicUdpProbeSettings(), hosts)
	if settings.ExpectedTargets != 60 || reason != "" || len(requests) != 120 {
		t.Fatalf("the %d row lb_alt matrix gave %q with %d requests, want 120", settings.ExpectedTargets, reason, len(requests))
	}
}
