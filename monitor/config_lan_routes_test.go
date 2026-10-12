package monitor

import (
	"maps"
	"testing"

	"gopkg.in/yaml.v3"
)

// The LAN hosts share one routes block through a yaml anchor; a host outside
// the LAN overrides the Redis hosts to their overlay addresses. Each host's
// LAN address must not depend on which block the settings map yields first.
func TestLanRoutesTakeTheRouteMostHostsAgreeOn(t *testing.T) {
	const lanBlock = `
by-us-fmt-5-edge-0: &us_fmt
    routes: &us_fmt_routes
        by-us-fmt-5-edge-5: 192.168.51.179
        by-us-fmt-5-edge-6: 192.168.51.193
        snow: 172.28.208.185
by-us-fmt-5-edge-5: *us_fmt
by-us-fmt-5-edge-6: *us_fmt
lan_hosts:
    by-us-fmt-5-edge-5:
        ip: 192.168.51.179
`
	lanRoutes := map[string]string{
		"by-us-fmt-5-edge-5": "192.168.51.179",
		"by-us-fmt-5-edge-6": "192.168.51.193",
		"snow":               "172.28.208.185",
	}
	tests := []struct {
		name     string
		settings string
		want     map[string]string
	}{
		{name: "LAN hosts only", settings: lanBlock, want: lanRoutes},
		{
			name: "VPN-only host overrides the Redis hosts",
			settings: lanBlock + `
sille:
    <<: *us_fmt
    routes:
        <<: *us_fmt_routes
        by-us-fmt-5-edge-5: 172.28.208.176
        by-us-fmt-5-edge-6: 172.28.208.177
`,
			want: lanRoutes,
		},
		{
			name: "overriding host sorts first",
			settings: lanBlock + `
a-vpn-only:
    routes:
        <<: *us_fmt_routes
        by-us-fmt-5-edge-5: 172.28.208.176
`,
			want: lanRoutes,
		},
		{
			name: "a tie goes to the first block by host name",
			settings: `
b-host:
    routes: {by-us-fmt-5-edge-5: 172.28.208.176}
a-host:
    routes: {by-us-fmt-5-edge-5: 192.168.51.179}
`,
			want: map[string]string{"by-us-fmt-5-edge-5": "192.168.51.179"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := map[string]any{}
			if err := yaml.Unmarshal([]byte(test.settings), &settings); err != nil {
				t.Fatal(err)
			}
			if routes := lanRoutesFromSettings(settings); !maps.Equal(routes, test.want) {
				t.Fatalf("lan routes = %v, want %v", routes, test.want)
			}
		})
	}
}
