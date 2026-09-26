package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const syntheticRouterName = "router-test"
const syntheticRouterBoot = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

const syntheticRouterSummary = `{"schema_version":1,"complete":true,"reason":"","running":{"complete":true,"changes":0,"deletes":0,"sets":0,"unverified":0,"protected_delete":false,"reason":"compared"},"saved":{"complete":true,"changes":0,"deletes":0,"sets":0,"unverified":0,"protected_delete":false,"reason":"compared"},"topology":{"complete":true,"reason":"","neighbors":[{"interface":"eth0","family":"ipv4","address":"192.0.2.1","role":"upstream"}],"conntrack":{"explicit":true,"table_size":1000,"hash_size":256}}}`

type syntheticRouterSource struct {
	*syntheticSource
	kind        string
	body        string
	summary     string
	desired     string
	boot        string
	err         error
	hostCalls   int
	localCalls  int
	missingEnd  bool
	changedBoot bool
}

func newSyntheticRouterSource() *syntheticRouterSource {
	self := &syntheticRouterSource{
		syntheticSource: &syntheticSource{},
		summary:         syntheticRouterSummary,
		desired:         "system {\n host-name router-test\n}\n",
		boot:            syntheticRouterBoot,
	}
	self.syntheticSource.localFn = func(name string, args ...string) (string, error) {
		self.localCalls++
		if name != "warpctl" || len(args) < 2 || args[0] != "vyos" {
			return "", errors.New("unexpected synthetic local request")
		}
		if args[1] == "create-config" {
			for _, arg := range args {
				if strings.HasPrefix(arg, "--out=") {
					return "", os.WriteFile(filepath.Join(strings.TrimPrefix(arg, "--out="), syntheticRouterName+"-config.boot"), []byte(self.desired), 0600)
				}
			}
		}
		if args[1] == "compare-config" {
			return self.summary, nil
		}
		return "", errors.New("unexpected synthetic local command")
	}
	return self
}

func (self *syntheticRouterSource) HostTimeout(ctx context.Context, h HostSettings, command string, timeout time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	self.hostCalls++
	if h.Name != syntheticRouterName || timeout <= 0 || timeout > 20*time.Second || !strings.Contains(command, "hostname -s") || !strings.Contains(command, "URN_ROUTER_END") {
		return "", errors.New("synthetic capture contract mismatch")
	}
	if self.err != nil {
		return self.body, self.err
	}
	after := self.boot
	if self.changedBoot {
		after = "ffffffff-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	}
	output := "URN_ROUTER_V1\nhostname=" + syntheticRouterName + "\nboot=" + self.boot + "\n--body--\n" + self.body + "\n--after--\nhostname=" + syntheticRouterName + "\nboot=" + after + "\nURN_ROUTER_END\n"
	if self.missingEnd {
		output = strings.TrimSuffix(output, "URN_ROUTER_END\n")
	}
	return output, nil
}

func syntheticRouterSettings(source *syntheticRouterSource, now *time.Time) SignalSettings {
	settings := syntheticSettings(source)
	settings.AddressMode = AddressModeOverlay
	settings.Routers = []RouterSettings{{Name: syntheticRouterName, LANAddress: "192.0.2.9", OverlayAddress: "198.51.100.9", SSHUser: "synthetic", SSHKeyPaths: []string{"/synthetic/key"}}}
	settings.Now = func() time.Time { return *now }
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return true, nil }
	return settings
}

func requireRouterPrivate(t *testing.T, alerts Alerts) {
	t.Helper()
	encoded, err := json.Marshal(alerts)
	if err != nil {
		t.Fatal(err)
	}
	var jsonl bytes.Buffer
	if err := alerts.WriteJSONL(&jsonl); err != nil {
		t.Fatal(err)
	}
	outputs := []string{string(encoded), jsonl.String()}
	for _, alert := range alerts {
		outputs = append(outputs, alert.Markdown())
	}
	for _, output := range outputs {
		for _, forbidden := range []string{"192.0.2.", "198.51.100.", "2001:db8", syntheticRouterBoot, "synthetic-secret", "/synthetic/key", "provider hostile", "eth0"} {
			if strings.Contains(output, forbidden) {
				t.Fatal("router observation leaked private fixture data")
			}
		}
	}
}

func TestRouterInventoryIsSeparateAndCloned(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	settings := syntheticRouterSettings(newSyntheticRouterSource(), &now)
	cfg := configFromSignalSettings(settings)
	if len(cfg.hosts) != len(settings.Hosts) || len(cfg.routers) != 1 {
		t.Fatal("router enrollment changed generic Linux host enumeration")
	}
	settings.Routers[0].SSHKeyPaths[0] = "changed"
	if cfg.routers[0].sshKeyPaths[0] != "/synthetic/key" {
		t.Fatal("router SSH identity slice aliases mutable settings")
	}
	for _, configured := range cfg.hosts {
		if configured.name == syntheticRouterName {
			t.Fatal("router entered generic host inventory")
		}
	}
}

func TestRouterExclusionsBlockSharedEndpointsBeforeSource(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	settings := syntheticRouterSettings(source, &now)
	settings.Routers = append(settings.Routers, RouterSettings{Name: "router-paused", OverlayAddress: "198.51.100.9", Disabled: true})
	alerts, err := NewRouterConfigSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if source.hostCalls != 0 {
		t.Fatal("shared disabled router endpoint reached source")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	settings.Routers = settings.Routers[:1]
	settings, err = ExcludeHosts(settings, syntheticRouterName)
	if err != nil {
		t.Fatal("explicit router exclusion was not recognized")
	}
	alerts, err = NewRouterConfigSignal().Run(context.Background(), settings)
	if err != nil || source.hostCalls != 0 {
		t.Fatal("excluded router reached source")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
}

func TestRouterObservationCancellationAndStaleGenerationMakeNoContact(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	settings := syntheticRouterSettings(source, &now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, signal := range []Signal{NewRouterConfigSignal(), NewRouterNeighborsSignal(), NewRouterConntrackSignal()} {
		if _, err := signal.Run(ctx, settings); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled router observation did not preserve cancellation")
		}
	}
	settings.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return false, nil }
	alerts, err := NewRouterConfigSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	if source.hostCalls != 0 || source.localCalls != 0 {
		t.Fatal("stale or canceled settings reached an observation transport")
	}
}

func TestRouterSettingsRejectAmbiguousOwnership(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	settings := syntheticRouterSettings(newSyntheticRouterSource(), &now)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: syntheticRouterName})
	if err := settings.validate(); err == nil {
		t.Fatal("router/generic host duplicate identity was accepted")
	}
	settings.Hosts = settings.Hosts[:len(settings.Hosts)-1]
	settings.Routers[0].OverlayAddress = "router-target.example"
	if err := settings.validate(); err == nil {
		t.Fatal("implicit DNS endpoint was accepted as explicit router address authority")
	}
}

func TestRouterDisabledHostPublicEndpointsRemainOwned(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	source := newSyntheticRouterSource()
	settings := syntheticRouterSettings(source, &now)
	settings.disabledHosts = []HostSettings{{
		Name: "paused-host", LANAddress: "192.0.2.80", OverlayAddress: "198.51.100.80",
		scopeEndpoints: []string{"paused-legacy.example"},
		PublicLB:       []PublicLBInterfaceSettings{{Interface: "public-test", IPv4Address: "198.51.100.9", IPv6Address: "2001:db8::80"}},
		Proxy:          &ProxyHostSettings{PublicHostname: "paused-proxy.example"},
	}}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.cfg.hosts) != len(settings.Hosts) {
		t.Fatal("disabled endpoint ownership changed enabled generic host denominator")
	}
	scoped, ok := env.runner.(*hostScopeRunner)
	if !ok {
		t.Fatal("disabled owner did not arm transport policy")
	}
	for _, endpoint := range []string{"192.0.2.80", "198.51.100.80", "198.51.100.9", "[2001:db8::80]:443", "https://paused-proxy.example/", "paused-legacy.example"} {
		if !hostScopeOnlyError(scoped.guardEndpoint(context.Background(), endpoint)) {
			t.Fatal("disabled owner endpoint escaped exact transport policy")
		}
	}
	alerts, err := NewRouterConfigSignal().Run(context.Background(), settings)
	if err != nil || source.hostCalls != 0 || source.localCalls != 0 {
		t.Fatal("router alias of a disabled public endpoint reached transport")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
}

func TestRouterAndPublicUdpSettingsDecodeCloneAndGeneration(t *testing.T) {
	var parsed monitorYaml
	if err := yaml.Unmarshal([]byte("routers:\n  - name: router-test\n    overlay_ip: 198.51.100.9\n    ssh_identity_files: [/synthetic/key]\npublic_udp:\n  enabled: false\n  expected_targets: 1\n  targets:\n    - name: path-test\n      host: paused-host\n      families: [ipv4, ipv6]\n"), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Routers) != 1 || parsed.Routers[0].SSHKeyPaths[0] != "/synthetic/key" || len(parsed.PublicUdp.Targets) != 1 {
		t.Fatal("new explicit YAML inventory seams did not decode")
	}
	settings := syntheticSettings(&syntheticSource{})
	settings.Routers, settings.PublicUdp = parsed.Routers, parsed.PublicUdp
	monitor := NewWithSignals(settings, NewRouterConfigSignal())
	settings.Routers[0].SSHKeyPaths[0] = "changed"
	settings.PublicUdp.Targets[0].Families[0] = "changed"
	if monitor.settings.Routers[0].SSHKeyPaths[0] != "/synthetic/key" || monitor.settings.PublicUdp.Targets[0].Families[0] != "ipv4" {
		t.Fatal("new nested settings alias caller-owned mutable values")
	}
	startup := monitor.settings
	current := startup
	current.Routers = cloneRouterSettings(startup.Routers)
	current.Routers[0].Disabled = true
	check := NewSettingsGenerationCheck(func() (SignalSettings, error) { return current, nil })
	if same, err := check(context.Background(), startup); err != nil || same {
		t.Fatal("router disable was absent from generation comparison")
	}
	current = startup
	current.routerDesiredGeneration[0]++
	if same, err := check(context.Background(), startup); err != nil || same {
		t.Fatal("router-only desired resource change was absent from generation comparison")
	}
}

func TestRouterOutputCapCancelsAndNeverAcceptsAValidPrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &routerLimitWriter{limit: 2, cancel: cancel}
	if _, err := writer.Write([]byte("{}trailing")); err == nil || !writer.exceeded || ctx.Err() == nil || writer.buffer.Len() != 2 {
		t.Fatal("output cap accepted a syntactically valid truncated prefix")
	}
	for _, raw := range []string{
		strings.Replace(syntheticRouterSummary, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		strings.Replace(syntheticRouterSummary, `"changes":0`, `"changes":1`, 1),
		strings.Replace(syntheticRouterSummary, `"complete":true`, `"complete":null`, 1),
		strings.Replace(syntheticRouterSummary, `"interface":"eth0"`, `"interface":"eth0","interface":"eth0"`, 1),
		syntheticRouterSummary + "{}",
	} {
		if _, err := parseRouterSummary(raw); err == nil {
			t.Fatal("malformed or ambiguous router summary was accepted")
		}
	}
}

func TestRouterRegistryAndReadOnlyCaptureProtocol(t *testing.T) {
	want := map[string]string{"router-config": "18.4", "router-neighbors": "18.5", "router-conntrack": "18.6"}
	for _, signal := range NewSignals() {
		if number, ok := want[signal.Key()]; ok {
			if signal.Number() != number || signal.Cadence() != 5*time.Minute {
				t.Fatal("router registry metadata does not match owned catalog")
			}
			delete(want, signal.Key())
		}
	}
	if len(want) != 0 {
		t.Fatal("router signals were not registered")
	}
	for _, kind := range []string{"config", "neighbors", "conntrack"} {
		command := routerCaptureCommand(syntheticRouterName, kind)
		gate := strings.Index(command, "[ \"$router_name\" = 'router-test' ]")
		body := strings.Index(command, "--body--")
		if gate < 0 || body < gate || !strings.Contains(command, "URN_ROUTER_END") || !strings.Contains(command, "router_after_boot") {
			t.Fatal("capture does not gate identity before reads and bracket completion")
		}
		for _, forbidden := range []string{"sudo", "python", "ip -j", " flush", " restart", "commit", " save", "ping "} {
			if strings.Contains(command, forbidden) {
				t.Fatal("capture exceeded its read-only portable command contract")
			}
		}
	}
}
