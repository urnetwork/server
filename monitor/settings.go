package monitor

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AddressMode selects which configured address is used for SSH.
type AddressMode string

const (
	AddressModeLAN     AddressMode = "lan"
	AddressModeOverlay AddressMode = "overlay"
)

// HostSettings describes one monitored host and its roles.
type HostSettings struct {
	Name           string
	LANAddress     string
	OverlayAddress string
	Roles          []string

	RedisEntryPort int
	RedisNodePorts []int
	// RedisExpectedReplicas arms SIGNALS.md §3.6 replica-cover. Zero is a
	// valid explicit dark state for today's no-replica topology.
	RedisExpectedReplicas int

	// Proxy arms SIGNALS.md §14.5 for this host. Live service allocations are
	// discovered from running containers on every probe; no dynamic port is
	// cached here.
	Proxy *ProxyHostSettings

	// EdgeIPv6 is derived from the active services.yml LB version. It is not
	// duplicated in monitor.yml, so public probes always compare the live host
	// with the same configured identity used by warpctl.
	EdgeIPv6 []EdgeIPv6InterfaceSettings
}

// EdgeIPv6InterfaceSettings describes one configured public IPv6 LB path.
// ProbeHostname supplies TLS SNI while Address pins the request to this exact
// interface instead of allowing DNS health selection to hide one failed edge.
type EdgeIPv6InterfaceSettings struct {
	Interface     string
	Address       string
	ProbeHostname string
}

// ProxyHostSettings contains only stable public/routing identity. Address
// families are "ipv4" and/or "ipv6"; both are checked when omitted.
type ProxyHostSettings struct {
	PublicHostname   string
	PublicInterface  string
	RoutingTable     int
	LoadBalancerUnit string
	AddressFamilies  []string
}

// PostgreSQLSettings contains the connection facts used by the remote psql
// transport. Password is sent on stdin, never as a command-line argument.
type PostgreSQLSettings struct {
	Port          int
	PgBouncerPort int
	User          string
	Password      string
	Database      string
}

// SourceAttributionSettings arms SIGNALS.md §8.8. Each configured expected
// address is checked through its family-specific endpoint from the monitor
// runner itself, so a healthy API process cannot hide lost client identity.
type SourceAttributionSettings struct {
	IPv4URL      string
	IPv6URL      string
	ExpectedIPv4 string
	ExpectedIPv6 string
}

// Row is one machine-readable PostgreSQL result row.
type Row []string

// SignalSource is the synthetic-test and alternate-transport seam for
// signals. Production leaves Source nil and uses the SSH implementation built
// from the remaining SignalSettings fields.
type SignalSource interface {
	PostgreSQL(ctx context.Context, query string) ([]Row, error)
	Redis(ctx context.Context, host HostSettings, port int, args ...string) (string, error)
	Host(ctx context.Context, host HostSettings, command string) (string, error)
}

// TimedSignalSource optionally supports commands whose timeout differs from
// SignalSettings.CommandTimeout.
type TimedSignalSource interface {
	HostTimeout(ctx context.Context, host HostSettings, command string, timeout time.Duration) (string, error)
}

// LocalSignalSource optionally provides local commands such as warpctl.
type LocalSignalSource interface {
	Local(ctx context.Context, name string, args ...string) (string, error)
}

// TCPExchangeSignalSource optionally supplies a synthetic or alternate raw
// TCP exchange. It is used for protocol-level probes where a listening socket
// alone is not proof that the expected service owns the public path.
type TCPExchangeSignalSource interface {
	TCPExchange(ctx context.Context, network, address string, payload []byte, responseBytes int) ([]byte, error)
}

// StreamingSignalSource optionally provides long-running local streams. It is
// used by the standing SIGNALS.md §1.5 log collector.
type StreamingSignalSource interface {
	StreamLocal(ctx context.Context, name string, args ...string) (*exec.Cmd, io.ReadCloser, error)
}

// SignalSettings is all runtime input shared by probes. SSHKeyPaths may hold
// multiple identity files for a mixed fleet; each becomes an explicit ssh -i
// option. Source can be supplied by tests to keep every probe synthetic.
type SignalSettings struct {
	Environment string
	// PublicDomain is the active services.yml domain used to construct
	// environment-scoped public health hostnames without duplicating them in
	// monitor.yml.
	PublicDomain string
	// VerificationEnabled is the canonical st-subsystem feature state. It lets
	// task probes distinguish a legitimately slow enabled verification job
	// from a stale recurring chain that must not exist while the subsystem is
	// disabled.
	VerificationEnabled bool

	SSHUser     string
	SSHDevUser  string
	SSHKeyPaths []string
	AddressMode AddressMode

	Hosts             []HostSettings
	PostgreSQL        PostgreSQLSettings
	SourceAttribution SourceAttributionSettings
	StateDir          string

	SSHConnectTimeout time.Duration
	CommandTimeout    time.Duration

	Source SignalSource
	Now    func() time.Time

	runtime *signalRuntime
}

type signalRuntime struct {
	baseline    *baselineStore
	baselineErr error
}

// ExcludeEdgeIPv6Hosts returns settings with exact public IPv6 probes disabled
// only for the named hosts. Other probes and other hosts remain enabled.
// Unknown names fail closed so an operational pause cannot silently miss its
// intended target.
func ExcludeEdgeIPv6Hosts(settings SignalSettings, names ...string) (SignalSettings, error) {
	requested := map[string]bool{}
	for _, name := range names {
		requested[name] = false
	}
	filtered := settings
	filtered.Hosts = append([]HostSettings(nil), settings.Hosts...)
	for i := range filtered.Hosts {
		if _, ok := requested[filtered.Hosts[i].Name]; !ok {
			continue
		}
		requested[filtered.Hosts[i].Name] = true
		filtered.Hosts[i].EdgeIPv6 = nil
	}
	for name, found := range requested {
		if !found {
			return SignalSettings{}, fmt.Errorf("monitor: excluded IPv6 host %q is not configured", name)
		}
	}
	return filtered, nil
}

func (s SignalSettings) withDefaults() SignalSettings {
	if s.AddressMode == "" {
		s.AddressMode = AddressModeOverlay
	}
	if s.PostgreSQL.Port == 0 {
		s.PostgreSQL.Port = 5432
	}
	if s.PostgreSQL.PgBouncerPort == 0 {
		s.PostgreSQL.PgBouncerPort = 6432
	}
	if s.SourceAttribution.IPv4URL == "" {
		s.SourceAttribution.IPv4URL = "https://api-v4.bringyour.com/my-ip-info"
	}
	if s.SourceAttribution.IPv6URL == "" {
		s.SourceAttribution.IPv6URL = "https://api-v6.bringyour.com/my-ip-info"
	}
	if s.SSHConnectTimeout <= 0 {
		s.SSHConnectTimeout = 10 * time.Second
	}
	if s.CommandTimeout <= 0 {
		s.CommandTimeout = 60 * time.Second
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	return s
}

func (s SignalSettings) withRuntime() SignalSettings {
	if s.runtime != nil || s.StateDir == "" {
		return s
	}
	baseline, err := newBaselineStore(filepath.Join(s.StateDir, "baseline"))
	s.runtime = &signalRuntime{baseline: baseline, baselineErr: err}
	return s
}

// Validate checks settings after applying the same defaults used by signals.
func (s SignalSettings) Validate() error { return s.withDefaults().validate() }

func (s SignalSettings) validate() error {
	if s.AddressMode != AddressModeLAN && s.AddressMode != AddressModeOverlay {
		return fmt.Errorf("monitor: unsupported address mode %q", s.AddressMode)
	}
	if s.Source != nil {
		return nil
	}
	if s.SSHUser == "" && s.SSHDevUser == "" {
		return fmt.Errorf("monitor: SSH user is required")
	}
	if len(s.Hosts) == 0 {
		return fmt.Errorf("monitor: at least one host is required")
	}
	return nil
}

func newProbeEnv(settings SignalSettings) (*probeEnv, error) {
	cfg := configFromSignalSettings(settings)
	var transport probeRunner
	if settings.Source != nil {
		transport = &sourceRunner{source: settings.Source}
	} else {
		transport = newRunner(cfg)
	}

	var baseline *baselineStore
	if settings.runtime != nil {
		if settings.runtime.baselineErr == nil {
			baseline = settings.runtime.baseline
		}
	} else if settings.StateDir != "" {
		// Baselines refine static bands but never gate direct probes. An
		// unavailable local state directory therefore degrades to static
		// thresholds, matching MONITOR.md §3.3.
		baseline, _ = newBaselineStore(filepath.Join(settings.StateDir, "baseline"))
	}
	return &probeEnv{cfg: cfg, runner: transport, baseline: baseline, now: settings.Now}, nil
}

func configFromSignalSettings(settings SignalSettings) *monitorConfig {
	cfg := &monitorConfig{
		env:                 settings.Environment,
		publicDomain:        settings.PublicDomain,
		verificationEnabled: settings.VerificationEnabled,
		sshUser:             settings.SSHUser,
		sshDevUser:          settings.SSHDevUser,
		sshKeyPaths:         append([]string(nil), settings.SSHKeyPaths...),
		addressMode:         string(settings.AddressMode),
		pgPort:              settings.PostgreSQL.Port,
		pgbouncerPort:       settings.PostgreSQL.PgBouncerPort,
		pgUser:              settings.PostgreSQL.User,
		pgPassword:          settings.PostgreSQL.Password,
		pgDb:                settings.PostgreSQL.Database,
		sourceIPv4URL:       settings.SourceAttribution.IPv4URL,
		sourceIPv6URL:       settings.SourceAttribution.IPv6URL,
		expectedSourceIPv4:  settings.SourceAttribution.ExpectedIPv4,
		expectedSourceIPv6:  settings.SourceAttribution.ExpectedIPv6,
		stateDir:            settings.StateDir,
		sshConnectTimeout:   settings.SSHConnectTimeout,
		commandTimeout:      settings.CommandTimeout,
	}
	for _, configured := range settings.Hosts {
		h := &host{
			name:                  configured.Name,
			lanIp:                 configured.LANAddress,
			overlayIp:             configured.OverlayAddress,
			roles:                 append([]string(nil), configured.Roles...),
			redisEntryPort:        configured.RedisEntryPort,
			redisExpectedReplicas: configured.RedisExpectedReplicas,
			proxy:                 cloneProxyHostSettings(configured.Proxy),
			edgeIPv6:              cloneEdgeIPv6Settings(configured.EdgeIPv6),
		}
		if len(configured.RedisNodePorts) > 0 {
			h.redisPorts = append([]int(nil), configured.RedisNodePorts...)
			h.redisNodeLo = configured.RedisNodePorts[0]
			h.redisNodeHi = configured.RedisNodePorts[len(configured.RedisNodePorts)-1]
		}
		cfg.hosts = append(cfg.hosts, h)
	}
	return cfg
}

func hostSettingsFromHost(h *host) HostSettings {
	if h == nil {
		return HostSettings{}
	}
	return HostSettings{
		Name:                  h.name,
		LANAddress:            h.lanIp,
		OverlayAddress:        h.overlayIp,
		Roles:                 append([]string(nil), h.roles...),
		RedisEntryPort:        h.redisEntryPort,
		RedisNodePorts:        h.redisNodePorts(),
		RedisExpectedReplicas: h.redisExpectedReplicas,
		Proxy:                 cloneProxyHostSettings(h.proxy),
		EdgeIPv6:              cloneEdgeIPv6Settings(h.edgeIPv6),
	}
}

func cloneProxyHostSettings(settings *ProxyHostSettings) *ProxyHostSettings {
	if settings == nil {
		return nil
	}
	clone := *settings
	clone.AddressFamilies = append([]string(nil), settings.AddressFamilies...)
	return &clone
}

func cloneEdgeIPv6Settings(settings []EdgeIPv6InterfaceSettings) []EdgeIPv6InterfaceSettings {
	return append([]EdgeIPv6InterfaceSettings(nil), settings...)
}

type sourceRunner struct {
	source SignalSource
}

func (r *sourceRunner) pg(ctx context.Context, sql string) ([]pgRow, error) {
	rows, err := r.source.PostgreSQL(ctx, sql)
	if err != nil {
		return nil, err
	}
	converted := make([]pgRow, len(rows))
	for i, row := range rows {
		converted[i] = pgRow(row)
	}
	return converted, nil
}

func (r *sourceRunner) redis(ctx context.Context, h *host, port int, args ...string) (string, error) {
	out, err := r.redisRaw(ctx, h, port, args...)
	return strings.TrimSpace(out), err
}

func (r *sourceRunner) redisRaw(ctx context.Context, h *host, port int, args ...string) (string, error) {
	return r.source.Redis(ctx, hostSettingsFromHost(h), port, args...)
}

func (r *sourceRunner) shell(ctx context.Context, h *host, command string) (string, error) {
	return r.source.Host(ctx, hostSettingsFromHost(h), command)
}

func (r *sourceRunner) sshTimeout(ctx context.Context, h *host, command string, stdin string, timeout time.Duration) (string, error) {
	if stdin != "" {
		return "", fmt.Errorf("monitor: synthetic SignalSource cannot provide host stdin")
	}
	if timed, ok := r.source.(TimedSignalSource); ok {
		return timed.HostTimeout(ctx, hostSettingsFromHost(h), command, timeout)
	}
	return r.source.Host(ctx, hostSettingsFromHost(h), command)
}

func (r *sourceRunner) local(ctx context.Context, name string, args ...string) (string, error) {
	local, ok := r.source.(LocalSignalSource)
	if !ok {
		return "", fmt.Errorf("monitor: SignalSource does not implement LocalSignalSource")
	}
	return local.Local(ctx, name, args...)
}

func (r *sourceRunner) tcpExchange(ctx context.Context, network, address string, payload []byte, responseBytes int) ([]byte, error) {
	source, ok := r.source.(TCPExchangeSignalSource)
	if !ok {
		return nil, fmt.Errorf("monitor: SignalSource does not implement TCPExchangeSignalSource")
	}
	return source.TCPExchange(ctx, network, address, payload, responseBytes)
}

func (r *sourceRunner) warpctl(ctx context.Context, args ...string) (string, error) {
	return r.local(ctx, "warpctl", args...)
}

func (r *sourceRunner) warpctlStream(ctx context.Context, args ...string) (*exec.Cmd, io.ReadCloser, error) {
	streaming, ok := r.source.(StreamingSignalSource)
	if !ok {
		return nil, nil, fmt.Errorf("monitor: SignalSource does not implement StreamingSignalSource")
	}
	return streaming.StreamLocal(ctx, "warpctl", args...)
}
