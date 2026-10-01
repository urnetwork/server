package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	routerConfigByteLimit    = 1024 * 1024
	routerDataByteLimit      = 256 * 1024
	routerCommandTimeout     = 20 * time.Second
	routerObservationTimeout = 45 * time.Second
	routerPairMaxAge         = 15 * time.Minute
)

// Explicit opt-in transport authority. No inferred LAN routes or generic
// Linux host roles are attached to these targets.
type RouterSettings struct {
	Name           string   `yaml:"name"`
	LANAddress     string   `yaml:"lan_ip"`
	OverlayAddress string   `yaml:"overlay_ip"`
	SSHUser        string   `yaml:"ssh_user"`
	SSHKeyPaths    []string `yaml:"ssh_identity_files"`
	Disabled       bool     `yaml:"disabled"`
}

func cloneRouterSettings(settings []RouterSettings) []RouterSettings {
	result := append([]RouterSettings(nil), settings...)
	for index := range result {
		result[index].SSHKeyPaths = append([]string(nil), result[index].SSHKeyPaths...)
	}
	return result
}

func cloneRouterScopeHosts(hosts []HostSettings) []HostSettings {
	result := append([]HostSettings(nil), hosts...)
	for index := range result {
		h := &result[index]
		h.Roles = append([]string(nil), h.Roles...)
		h.scopeEndpoints = append([]string(nil), h.scopeEndpoints...)
		h.SSHKeyPaths = append([]string(nil), h.SSHKeyPaths...)
		h.RedisNodePorts = append([]int(nil), h.RedisNodePorts...)
		h.Proxy = cloneProxyHostSettings(h.Proxy)
		h.EdgeIPv6 = cloneEdgeIPv6Settings(h.EdgeIPv6)
		h.PublicLB = clonePublicLBSettings(h.PublicLB)
		h.Subtensor = cloneSubtensorHostSettings(h.Subtensor)
		h.Backup = cloneBackupHostSettings(h.Backup)
	}
	return result
}

func validateRouterSettings(settings SignalSettings) error {
	names := map[string]bool{}
	for _, configured := range append(append([]HostSettings(nil), settings.Hosts...), settings.disabledHosts...) {
		names[configured.Name] = true
	}
	if len(settings.Routers) > 64 {
		return errors.New("monitor: router inventory exceeds bounded target count")
	}
	for _, configured := range settings.Routers {
		if !validDNSLabel(configured.Name) || names[configured.Name] {
			return errors.New("monitor: invalid or ambiguous router identity")
		}
		names[configured.Name] = true
		for _, value := range []string{configured.LANAddress, configured.OverlayAddress} {
			if value == "" {
				continue
			}
			address, err := netip.ParseAddr(value)
			if err != nil || address.Zone() != "" || address.Is4In6() || address.IsUnspecified() || address.IsMulticast() {
				return errors.New("monitor: router endpoint must be one explicit unicast address")
			}
		}
		if !configured.Disabled && (settings.AddressMode == AddressModeLAN && configured.LANAddress == "" || settings.AddressMode != AddressModeLAN && configured.OverlayAddress == "") {
			return errors.New("monitor: router address authority is unavailable")
		}
		if strings.ContainsAny(configured.SSHUser, " @:\r\n\x00") || strings.HasPrefix(configured.SSHUser, "-") {
			return errors.New("monitor: invalid router SSH principal")
		}
	}
	return nil
}

func (self *monitorConfig) scopeHosts() []*host {
	result := append([]*host(nil), self.hosts...)
	result = append(result, self.routers...)
	return append(result, self.disabledHosts...)
}

func (self *monitorConfig) hasDisabledRouter() bool {
	for _, h := range self.routers {
		if h.disabled {
			return true
		}
	}
	return false
}

// Separate bounded adapters preserve existing probes' transport contracts.
type routerRunner interface {
	routerHost(context.Context, *host, string, int) (string, error)
	routerLocal(context.Context, int, string, ...string) (string, error)
}

type routerLimitWriter struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (self *routerLimitWriter) Write(value []byte) (int, error) {
	remaining := self.limit - self.buffer.Len()
	if len(value) > remaining {
		_, _ = self.buffer.Write(value[:max(0, remaining)])
		self.exceeded = true
		self.cancel()
		return max(0, remaining), errors.New("router observation output limit exceeded")
	}
	return self.buffer.Write(value)
}

func runRouterCommand(ctx context.Context, limit int, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, routerCommandTimeout)
	defer cancel()
	stdout := &routerLimitWriter{limit: limit, cancel: cancel}
	stderr := &routerLimitWriter{limit: 64 * 1024, cancel: cancel}
	command := exec.CommandContext(ctx, name, args...)
	command.WaitDelay = time.Second
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded {
		return "", errors.New("router observation output limit exceeded")
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		// Neither raw stderr nor command arguments cross the reducer boundary.
		return "", fmt.Errorf("router observation command failed: %w", err)
	}
	return stdout.buffer.String(), nil
}

func (self *runner) routerHost(ctx context.Context, h *host, command string, limit int) (string, error) {
	bounded := *self
	bounded.runSSH = func(ctx context.Context, args []string, stdin string) (string, string, error) {
		if stdin != "" {
			return "", "", errors.New("router observation does not accept stdin")
		}
		output, err := runRouterCommand(ctx, limit, "ssh", args...)
		return output, "", err
	}
	return bounded.sshTimeout(ctx, h, command, "", routerCommandTimeout)
}

func (self *runner) routerLocal(ctx context.Context, limit int, name string, args ...string) (string, error) {
	return runRouterCommand(ctx, limit, name, args...)
}

func (self *sourceRunner) routerHost(ctx context.Context, h *host, command string, limit int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, routerCommandTimeout)
	defer cancel()
	output, err := self.sshTimeout(ctx, h, command, "", routerCommandTimeout)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(output) > limit {
		return "", errors.New("router observation output limit exceeded")
	}
	return output, err
}

func (self *sourceRunner) routerLocal(ctx context.Context, limit int, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, routerCommandTimeout)
	defer cancel()
	output, err := self.local(ctx, name, args...)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(output) > limit {
		return "", errors.New("router observation output limit exceeded")
	}
	return output, err
}

func (self *hostScopeRunner) routerHost(ctx context.Context, h *host, command string, limit int) (string, error) {
	if err := self.guardSshHost(ctx, h); err != nil {
		return "", err
	}
	bounded, ok := self.probeRunner.(routerRunner)
	if !ok {
		return "", errors.New("bounded router source unavailable")
	}
	return bounded.routerHost(ctx, h, command, limit)
}

func (self *hostScopeRunner) routerLocal(ctx context.Context, limit int, name string, args ...string) (string, error) {
	index := 2
	if len(args) >= 2 && args[1] == "create-config" {
		index = 3
	}
	if name != "warpctl" || len(args) <= index || args[0] != "vyos" || args[1] != "create-config" && args[1] != "compare-config" {
		return "", errors.New("router local command is not read-only")
	}
	found := false
	for _, h := range self.cfg.routers {
		if h.name == args[index] {
			found = true
			if err := self.guardSshHost(ctx, h); err != nil {
				return "", err
			}
		}
	}
	if !found {
		return "", errors.New("router local command has no inventory owner")
	}
	bounded, ok := self.probeRunner.(routerRunner)
	if !ok {
		return "", errors.New("bounded router source unavailable")
	}
	return bounded.routerLocal(ctx, limit, name, args...)
}

type routerComparison struct {
	Complete        bool   `json:"complete"`
	Changes         int    `json:"changes"`
	Deletes         int    `json:"deletes"`
	Sets            int    `json:"sets"`
	Unverified      int    `json:"unverified"`
	ProtectedDelete bool   `json:"protected_delete"`
	Reason          string `json:"reason"`
}

type routerNeighborTarget struct {
	Interface string `json:"interface"`
	Family    string `json:"family"`
	Address   string `json:"address"`
	Role      string `json:"role"`
}

type routerCapacity struct {
	Explicit  bool   `json:"explicit"`
	TableSize uint64 `json:"table_size"`
	HashSize  uint64 `json:"hash_size"`
}

type routerSummary struct {
	SchemaVersion int              `json:"schema_version"`
	Complete      bool             `json:"complete"`
	Reason        string           `json:"reason"`
	Running       routerComparison `json:"running"`
	Saved         routerComparison `json:"saved"`
	Topology      struct {
		Complete  bool                   `json:"complete"`
		Reason    string                 `json:"reason"`
		Neighbors []routerNeighborTarget `json:"neighbors"`
		Conntrack routerCapacity         `json:"conntrack"`
	} `json:"topology"`
}

func routerJsonFields(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return nil, errors.New("router summary shape invalid")
	}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("router summary field invalid")
		}
		if _, exists := fields[name]; exists {
			return nil, errors.New("router summary field duplicated")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("router summary value invalid")
		}
		fields[name] = value
	}
	close, err := decoder.Token()
	var trailing json.RawMessage
	if err != nil || close != json.Delim('}') || decoder.Decode(&trailing) != io.EOF || len(fields) != len(keys) {
		return nil, errors.New("router summary shape incomplete")
	}
	for _, key := range keys {
		if value, ok := fields[key]; !ok || bytes.Equal(value, []byte("null")) {
			return nil, errors.New("router summary field unavailable")
		}
	}
	return fields, nil
}

var routerInterfacePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,15}$`)
var routerBootPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func parseRouterSummary(raw string) (routerSummary, error) {
	var result routerSummary
	if len(raw) > routerDataByteLimit {
		return result, errors.New("router summary limit exceeded")
	}
	fields, err := routerJsonFields([]byte(raw), "schema_version", "complete", "reason", "running", "saved", "topology")
	if err != nil {
		return result, err
	}
	for _, name := range []string{"running", "saved"} {
		if _, err := routerJsonFields(fields[name], "complete", "changes", "deletes", "sets", "unverified", "protected_delete", "reason"); err != nil {
			return result, err
		}
	}
	topology, err := routerJsonFields(fields["topology"], "complete", "reason", "neighbors", "conntrack")
	if err != nil {
		return result, err
	}
	if _, err := routerJsonFields(topology["conntrack"], "explicit", "table_size", "hash_size"); err != nil {
		return result, err
	}
	var neighbors []json.RawMessage
	if json.Unmarshal(topology["neighbors"], &neighbors) != nil {
		return result, errors.New("router summary neighbor list invalid")
	}
	for _, neighbor := range neighbors {
		if _, err := routerJsonFields(neighbor, "interface", "family", "address", "role"); err != nil {
			return result, err
		}
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.SchemaVersion != 1 {
		return routerSummary{}, errors.New("router summary schema invalid")
	}
	for _, comparison := range []routerComparison{result.Running, result.Saved} {
		if comparison.Changes < 0 || comparison.Deletes < 0 || comparison.Sets < 0 || comparison.Unverified < 0 || comparison.Changes != comparison.Deletes+comparison.Sets || comparison.ProtectedDelete && comparison.Deletes == 0 {
			return routerSummary{}, errors.New("router comparison counts invalid")
		}
	}
	seen := map[string]bool{}
	if len(result.Topology.Neighbors) > 1024 {
		return routerSummary{}, errors.New("router neighbor denominator invalid")
	}
	for _, neighbor := range result.Topology.Neighbors {
		address, err := netip.ParseAddr(neighbor.Address)
		key := neighbor.Interface + "/" + neighbor.Family + "/" + neighbor.Address
		if err != nil || address.Zone() != "" || address.Is4In6() || address.IsUnspecified() || address.IsMulticast() || !routerInterfacePattern.MatchString(neighbor.Interface) ||
			(neighbor.Family != "ipv4" && neighbor.Family != "ipv6") || (neighbor.Family == "ipv4") != address.Is4() || (neighbor.Role != "upstream" && neighbor.Role != "port") || seen[key] {
			return routerSummary{}, errors.New("router neighbor authority invalid")
		}
		seen[key] = true
	}
	return result, nil
}

type routerObservation struct {
	boot       string
	body       string
	summary    routerSummary
	generation [32]byte
	at         time.Time
}

func routerCaptureCommand(name, kind string) string {
	body := ""
	switch kind {
	case "config":
		body = "printf '%s\\n' '--running--'\n/opt/vyatta/bin/vyatta-op-cmd-wrapper show configuration\nprintf '\\n%s\\n' '--saved--'\ncat /config/config.boot"
	case "neighbors":
		body = "ip -s neigh show nud all"
	case "conntrack":
		body = "printf '%s\\n' '--count--'\ncat /proc/sys/net/netfilter/nf_conntrack_count\nprintf '%s\\n' '--max--'\ncat /proc/sys/net/netfilter/nf_conntrack_max\nprintf '%s\\n' '--hash--'\ncat /sys/module/nf_conntrack/parameters/hashsize\nprintf '%s\\n' '--stat--'\ncat /proc/net/stat/nf_conntrack"
	default:
		return ""
	}
	// The local pipe cap, native exit status, strict sections and final marker
	// all must succeed; a syntactically valid prefix is not a complete capture.
	return "set -eu\nexport LC_ALL=C\nrouter_name=$(hostname -s)\n[ \"$router_name\" = '" + name + "' ] || exit 21\nrouter_boot=$(cat /proc/sys/kernel/random/boot_id)\nprintf 'URN_ROUTER_V1\\nhostname=%s\\nboot=%s\\n--body--\\n' \"$router_name\" \"$router_boot\"\n" + body + "\nrouter_after_name=$(hostname -s)\nrouter_after_boot=$(cat /proc/sys/kernel/random/boot_id)\n[ \"$router_name\" = \"$router_after_name\" ] && [ \"$router_boot\" = \"$router_after_boot\" ] || exit 22\nprintf '\\n--after--\\nhostname=%s\\nboot=%s\\nURN_ROUTER_END\\n' \"$router_after_name\" \"$router_after_boot\""
}

func parseRouterCapture(raw, name string, limit int) (string, string, error) {
	if len(raw) > limit || !strings.HasPrefix(raw, "URN_ROUTER_V1\nhostname="+name+"\nboot=") || !strings.HasSuffix(raw, "\nURN_ROUTER_END\n") {
		return "", "", errors.New("router capture incomplete")
	}
	header, tail, ok := strings.Cut(raw, "\n--body--\n")
	if !ok {
		return "", "", errors.New("router capture header invalid")
	}
	lines := strings.Split(header, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[2], "boot=") {
		return "", "", errors.New("router capture identity invalid")
	}
	boot := strings.TrimPrefix(lines[2], "boot=")
	if !routerBootPattern.MatchString(boot) {
		return "", "", errors.New("router capture boot invalid")
	}
	body, after, ok := strings.Cut(tail, "\n--after--\n")
	if !ok || after != "hostname="+name+"\nboot="+boot+"\nURN_ROUTER_END\n" {
		return "", "", errors.New("router capture generation changed")
	}
	return body, boot, nil
}

func routerReadPrivate(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > routerConfigByteLimit {
		return nil, errors.New("router desired artifact unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, routerConfigByteLimit+1))
	if err != nil || len(value) == 0 || len(value) > routerConfigByteLimit {
		return nil, errors.New("router desired artifact incomplete")
	}
	return value, nil
}

func observeRouter(ctx context.Context, env *probeEnv, h *host, kind string) (routerObservation, error) {
	var result routerObservation
	ctx, cancel := context.WithTimeout(ctx, routerObservationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		if err := scoped.guardSshHost(ctx, h); err != nil {
			return result, err
		}
	}
	if env.cfg.routerGenerationCheck == nil {
		return result, errors.New("router settings generation unobservable")
	}
	if current, err := env.cfg.routerGenerationCheck(ctx); err != nil || !current {
		return result, errors.New("router settings generation stale or unavailable")
	}
	transport, ok := env.runner.(routerRunner)
	if !ok {
		return result, errors.New("bounded router observation unavailable")
	}
	dir, err := os.MkdirTemp("", "monitor-router-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(dir)
	if _, err := transport.routerLocal(ctx, routerDataByteLimit, "warpctl", "vyos", "create-config", env.cfg.env, h.name, "--out="+dir); err != nil {
		return result, err
	}
	desiredPath := filepath.Join(dir, h.name+"-config.boot")
	desired, err := routerReadPrivate(desiredPath)
	if err != nil {
		return result, err
	}
	result.generation = sha256.Sum256(desired)
	if kind == "config" {
		raw, err := transport.routerHost(ctx, h, routerCaptureCommand(h.name, kind), 2*routerConfigByteLimit+4096)
		if err != nil {
			return result, err
		}
		result.body, result.boot, err = parseRouterCapture(raw, h.name, 2*routerConfigByteLimit+4096)
		if err != nil {
			return result, err
		}
		running, saved, ok := strings.Cut(strings.TrimPrefix(result.body, "--running--\n"), "\n--saved--\n")
		if !strings.HasPrefix(result.body, "--running--\n") || !ok || running == "" || saved == "" || len(running) > routerConfigByteLimit || len(saved) > routerConfigByteLimit {
			return result, errors.New("router config capture incomplete")
		}
		for path, value := range map[string]string{h.name + "-live.config": running, h.name + "-saved.config": saved} {
			if err := os.WriteFile(filepath.Join(dir, path), []byte(value), 0600); err != nil {
				return result, err
			}
		}
	}
	summary, err := transport.routerLocal(ctx, routerDataByteLimit, "warpctl", "vyos", "compare-config", h.name, "--desired="+dir, "--in="+dir)
	if err != nil {
		return result, err
	}
	result.summary, err = parseRouterSummary(summary)
	if err != nil {
		return result, err
	}
	afterDesired, err := routerReadPrivate(desiredPath)
	if err != nil || !bytes.Equal(desired, afterDesired) {
		return result, errors.New("router desired artifact changed")
	}
	if kind != "config" {
		if kind == "neighbors" && (!result.summary.Topology.Complete || len(result.summary.Topology.Neighbors) == 0) {
			return result, errors.New("router desired observation authority incomplete")
		}
		raw, err := transport.routerHost(ctx, h, routerCaptureCommand(h.name, kind), routerDataByteLimit)
		if err != nil {
			return result, err
		}
		result.body, result.boot, err = parseRouterCapture(raw, h.name, routerDataByteLimit)
		if err != nil {
			return result, err
		}
	}
	if current, err := env.cfg.routerGenerationCheck(ctx); err != nil || !current {
		return result, errors.New("router settings generation changed")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.at = env.now()
	return result, nil
}

func routerUnknown(h *host, kind, reason string, err error) finding {
	if err == nil {
		err = errors.New("router observation incomplete")
	}
	f := cannotObserveFinding(h.name+"/router-"+kind, err)
	f.frame = kind
	f.observed += " observation_state=" + reason
	f.evidence = "Only fixed observation states and counts are public; raw configurations, neighbor identities, boot identifiers, paths and command output stay private."
	f.context = "Unknown is not convergence, outage, atomic capture, hardware acceptance or recovery. A fresh complete sample is required."
	f.playbook = "SIGNALS.md §§18.4–18.6"
	return f
}

func routerTargets(ctx context.Context, env *probeEnv, kind string, evaluate func(*host, routerObservation, error) []finding) ([]finding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(env.cfg.routers) == 0 {
		return []finding{{probeId: "router/" + kind, tier: tierWarn, class: "router-observation-unconfigured", target: "router-inventory", frame: kind, sustain: 1,
			symptom: "No routers are explicitly enrolled for this observation.", baseline: "Explicit operator-owned router inventory enables this read-only observation.", observed: "enrolled_routers=0 observation_enabled=false", mechanism: "Router inventory is independent of generic Linux hosts; absence cannot establish router health.", action: "Have the operator review router inventory and authorization before enrollment. Do not infer targets from alerts or services.", verify: "An authorized enrolled target produces complete bounded observations.", playbook: "SIGNALS.md §§18.4–18.6"}}, nil
	}
	budgetCtx, cancel := context.WithTimeout(ctx, routerObservationTimeout)
	defer cancel()
	type result struct {
		observation routerObservation
		err         error
	}
	results := make([]result, len(env.cfg.routers))
	semaphore := make(chan struct{}, 2)
	var wait sync.WaitGroup
	for index, h := range env.cfg.routers {
		wait.Add(1)
		go func(index int, h *host) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-budgetCtx.Done():
				results[index].err = budgetCtx.Err()
				return
			}
			results[index].observation, results[index].err = observeRouter(budgetCtx, env, h, kind)
		}(index, h)
	}
	wait.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	findings := []finding{}
	for index, h := range env.cfg.routers {
		findings = append(findings, evaluate(h, results[index].observation, results[index].err)...)
	}
	return findings, nil
}

func routerPairValid(previous, current routerObservation) bool {
	elapsed := current.at.Sub(previous.at)
	return previous.boot != "" && previous.boot == current.boot && previous.generation == current.generation && elapsed >= time.Second && elapsed <= routerPairMaxAge
}
