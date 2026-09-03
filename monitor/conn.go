// Transport to the production environment.
//
// Every SIGNALS.md command is written as a run-on-the-host command, and that
// is exactly how the monitor executes it: ssh into the host and run psql /
// redis-cli / shell there. ssh authentication is delegated to ~/.ssh/config
// (assumed set up), so no key material lives in the monitor. warpctl runs
// locally on the monitor machine (fleet-wide log reads, version registry).
// Direct tcp connectors (pgx, go-redis) are a later in-lan optimization
// behind the same helpers.
//
// Everything here is read-only against production: pg sessions set
// default_transaction_read_only, and only observational shell commands are
// ever run. Every command carries a hard timeout — a probe that times out is
// an observation (often the strongest one, e.g. a wedged redis ping), never a
// hot retry. All functions here are safe for concurrent use.
package monitor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// address modes select which host address the monitor dials
	addressModeLan     = "lan"     // deployed in-environment
	addressModeOverlay = "overlay" // local dev over the vpn

	// Keep the monitor below OpenSSH's default MaxStartups=10 even when one
	// signal fans out internally and other signal cadences collide. The budget
	// is per destination host, so unrelated hosts remain observable in
	// parallel. It is shared by every probe through signalRuntime.
	maxConcurrentRemoteCommandsPerHost = 2
)

// hostCommandLimiter is the monitor-wide SSH admission budget. Probe-local
// semaphores bound their own fan-out but cannot account for other probes; this
// transport-level limiter is therefore the final authority before a new SSH
// handshake starts.
type hostCommandLimiter struct {
	limit int

	mu    sync.Mutex
	slots map[string]chan struct{}
}

func newHostCommandLimiter(limit int) *hostCommandLimiter {
	if limit <= 0 {
		limit = maxConcurrentRemoteCommandsPerHost
	}
	return &hostCommandLimiter{
		limit: limit,
		slots: map[string]chan struct{}{},
	}
}

func (l *hostCommandLimiter) hostSlots(host string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.slots == nil {
		l.slots = map[string]chan struct{}{}
	}
	slots := l.slots[host]
	if slots == nil {
		limit := l.limit
		if limit <= 0 {
			limit = maxConcurrentRemoteCommandsPerHost
		}
		slots = make(chan struct{}, limit)
		l.slots[host] = slots
	}
	return slots
}

func (l *hostCommandLimiter) acquire(ctx context.Context, host string) (func(), error) {
	slots := l.hostSlots(host)
	select {
	case slots <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-slots })
		}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// host is one monitored host from the inventory (vault/<env>/monitor.yml).
type host struct {
	name      string
	lanIp     string // resolved from config settings.yml routes (lan mode)
	overlayIp string // from monitor.yml (overlay mode)
	roles     []string
	// redis-cluster hosts only
	redisEntryPort        int
	redisPorts            []int
	redisNodeLo           int
	redisNodeHi           int
	redisExpectedReplicas int
	proxy                 *ProxyHostSettings
	edgeIPv6              []EdgeIPv6InterfaceSettings
	publicLB              []PublicLBInterfaceSettings
	subtensor             *SubtensorHostSettings
	backup                *BackupHostSettings
}

func (self *host) hasRole(role string) bool {
	for _, r := range self.roles {
		if r == role {
			return true
		}
	}
	return false
}

// addr returns the address to dial for the given address mode.
func (self *host) addr(mode string) string {
	if mode == addressModeOverlay {
		return self.overlayIp
	}
	return self.lanIp
}

// redisNodePorts is the inclusive port range of cluster nodes on this host.
func (self *host) redisNodePorts() []int {
	if len(self.redisPorts) > 0 {
		return append([]int(nil), self.redisPorts...)
	}
	if self.redisNodeLo == 0 || self.redisNodeHi < self.redisNodeLo {
		return nil
	}
	ports := make([]int, 0, self.redisNodeHi-self.redisNodeLo+1)
	for p := self.redisNodeLo; p <= self.redisNodeHi; p += 1 {
		ports = append(ports, p)
	}
	return ports
}

// monitorConfig is the monitor's view of the environment
// (from monitor.yml + pg.yml + config settings.yml).
type monitorConfig struct {
	env                 string              // WARP_ENV
	publicDomain        string              // active services.yml domain
	websiteDomain       string              // canonical managed product website, when present
	managerHostname     string              // configured manager alias, when exposed
	logServices         []string            // active services.yml service inventory
	logServiceBlocks    map[string][]string // active per-service block inventory
	verificationEnabled bool

	sshUser     string // deployed login user
	sshDevUser  string // login user for local dev over the overlay
	sshKeyPaths []string
	addressMode string

	hosts []*host

	pgPort        int
	pgbouncerPort int
	pgUser        string
	pgPassword    string
	pgDb          string

	grafanaAdminPassword string

	sourceIPv4URL      string
	sourceIPv6URL      string
	expectedSourceIPv4 string
	expectedSourceIPv6 string

	// state dir for baselines and other local persistence
	stateDir string

	// hard timeouts; a command exceeding these is recorded as unreachable
	sshConnectTimeout time.Duration
	commandTimeout    time.Duration

	// Shared across the fresh probe environments created by Signal.Run. It
	// bounds actual SSH handshakes rather than just top-level signal calls.
	remoteCommands *hostCommandLimiter
}

func (self *monitorConfig) activeSshUser() string {
	if self.addressMode == addressModeOverlay && self.sshDevUser != "" {
		return self.sshDevUser
	}
	return self.sshUser
}

// hostsWithRole returns every host carrying the role.
func (self *monitorConfig) hostsWithRole(role string) []*host {
	hosts := []*host{}
	for _, h := range self.hosts {
		if h.hasRole(role) {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// hostByRole returns the first host carrying the role, or nil.
func (self *monitorConfig) hostByRole(role string) *host {
	if hosts := self.hostsWithRole(role); len(hosts) > 0 {
		return hosts[0]
	}
	return nil
}

// runner executes commands on hosts over ssh, and warpctl locally.
type runner struct {
	cfg            *monitorConfig
	remoteCommands *hostCommandLimiter
	runSSH         sshCommandRunner
}

func newRunner(cfg *monitorConfig) *runner {
	remoteCommands := cfg.remoteCommands
	if remoteCommands == nil {
		remoteCommands = newHostCommandLimiter(maxConcurrentRemoteCommandsPerHost)
	}
	return &runner{
		cfg:            cfg,
		remoteCommands: remoteCommands,
		runSSH:         runSSHCommand,
	}
}

type sshCommandRunner func(ctx context.Context, args []string, stdin string) (stdout string, stderr string, err error)

func runSSHCommand(ctx context.Context, args []string, stdin string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	return out.String(), errOut.String(), err
}

// unreachableError wraps an error where the command could not be run or timed
// out. The monitor treats this distinctly from a command that ran and returned
// a value: it means the target is currently unobservable.
type unreachableError struct {
	host string
	err  error
}

func (self *unreachableError) Error() string {
	return fmt.Sprintf("unreachable %s: %s", self.host, self.err)
}

// ssh runs remoteCmd on h, feeding stdin, returning stdout. It applies the
// config's hard timeout; a timeout or missing address is returned as
// *unreachableError.
func (self *runner) ssh(ctx context.Context, h *host, remoteCmd string, stdin string) (string, error) {
	return self.sshTimeout(ctx, h, remoteCmd, stdin, self.cfg.commandTimeout)
}

// sshTimeout is ssh with an explicit per-command timeout, for the few
// deliberately slow reads (the daily keyspace scan) that exceed the default
// budget.
func (self *runner) sshTimeout(ctx context.Context, h *host, remoteCmd string, stdin string, timeout time.Duration) (string, error) {
	addr := h.addr(self.cfg.addressMode)
	if addr == "" {
		return "", &unreachableError{host: h.name, err: fmt.Errorf("no address for mode %q", self.cfg.addressMode)}
	}

	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	connectTimeout := self.cfg.sshConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	release, err := self.remoteCommands.acquire(ctx, addr)
	if err != nil {
		return "", &unreachableError{host: h.name, err: fmt.Errorf("waiting for remote command slot: %w", err)}
	}
	defer release()

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := fmt.Sprintf("%s@%s", self.cfg.activeSshUser(), addr)
	sshArgs := self.sshArgs(target, remoteCmd, connectTimeout)
	out, errOut, err := self.runSSH(cmdCtx, sshArgs, stdin)
	if cmdCtx.Err() == context.DeadlineExceeded {
		return out, &unreachableError{host: h.name, err: fmt.Errorf("timeout after %s", timeout)}
	}
	if err != nil {
		// ssh dial failures (exit 255) mean the host itself is unobservable;
		// a nonzero exit from the remote command is a command error. Both
		// surface with the stderr text.
		return out, fmt.Errorf("%s: %w: %s", h.name, err, strings.TrimSpace(errOut))
	}
	return out, nil
}

func (self *runner) sshArgs(target, remoteCmd string, connectTimeout time.Duration) []string {
	sshArgs := []string{
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(connectTimeout.Seconds())),
		"-o", "StrictHostKeyChecking=accept-new",
	}
	for _, keyPath := range self.cfg.sshKeyPaths {
		if strings.TrimSpace(keyPath) != "" {
			sshArgs = append(sshArgs, "-i", keyPath)
		}
	}
	sshArgs = append(sshArgs, target, remoteCmd)
	return sshArgs
}

// pg runs a read-only sql battery on the pg-primary host, direct to 5432
// (never through pgbouncer — under load pgbouncer kills queued clients with
// query_wait_timeout while direct connects fine, SIGNALS.md 5.8). The password
// is passed on stdin line 1, never on argv. The read-only guard and statement
// timeout are applied via PGOPTIONS at connection, not as inline set
// statements — set prints a command tag to stdout that would pollute the
// parsed rows. Rows come back split on '|' (psql -A -F'|' -t).
func (self *runner) pg(ctx context.Context, sql string) ([]pgRow, error) {
	h := self.cfg.hostByRole("pg-primary")
	if h == nil {
		return nil, fmt.Errorf("no pg-primary host in inventory")
	}
	remoteCmd := fmt.Sprintf(
		"IFS= read -r PGPASSWORD; export PGPASSWORD; "+
			"export PGOPTIONS='-c statement_timeout=30000 -c default_transaction_read_only=on'; "+
			"exec psql -h localhost -p %d -U %s %s -X -A -F'|' -t -v ON_ERROR_STOP=1 -f -",
		self.cfg.pgPort, self.cfg.pgUser, self.cfg.pgDb,
	)
	stdin := self.cfg.pgPassword + "\n" + sql
	out, err := self.ssh(ctx, h, remoteCmd, stdin)
	if err != nil {
		return nil, err
	}
	return parsePgRows(out), nil
}

// pgRow is one psql output row, its cells split on '|'.
type pgRow []string

func (self pgRow) str(i int) string {
	if i < 0 || i >= len(self) {
		return ""
	}
	return strings.TrimSpace(self[i])
}

func parsePgRows(out string) []pgRow {
	rows := []pgRow{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		rows = append(rows, pgRow(strings.Split(line, "|")))
	}
	return rows
}

// redis runs redis-cli against a node port on a redis host and returns raw
// stdout. args are the redis-cli arguments (e.g. "CLUSTER", "INFO").
func (self *runner) redis(ctx context.Context, h *host, port int, args ...string) (string, error) {
	out, err := self.redisRaw(ctx, h, port, args...)
	return strings.TrimSpace(out), err
}

// redisRaw preserves binary command output. redis-cli appends a newline, but
// decoders such as encoding/gob safely stop after their value; trimming all
// whitespace could corrupt a legitimate first/last payload byte.
func (self *runner) redisRaw(ctx context.Context, h *host, port int, args ...string) (string, error) {
	remoteCmd := fmt.Sprintf("redis-cli -p %d %s", port, strings.Join(args, " "))
	return self.ssh(ctx, h, remoteCmd, "")
}

// shell runs an observational shell command on a host (top, ss, dmesg,
// journalctl, docker ps).
func (self *runner) shell(ctx context.Context, h *host, remoteCmd string) (string, error) {
	return self.ssh(ctx, h, remoteCmd, "")
}

// local runs a bounded command on the monitor machine. Output is combined
// stdout+stderr because several operational tools emit useful structured
// context on stderr even on success.
func (self *runner) local(ctx context.Context, name string, args ...string) (string, error) {
	timeout := self.cfg.commandTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("%s timeout after %s", name, timeout)
	}
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.String(), nil
}

func (self *runner) tcpExchange(ctx context.Context, network, address string, payload []byte, responseBytes int) ([]byte, error) {
	timeout := self.cfg.commandTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	connection, err := (&net.Dialer{Timeout: timeout}).DialContext(commandCtx, network, address)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		if _, err := connection.Write(payload); err != nil {
			return nil, err
		}
	}
	response := make([]byte, responseBytes)
	if responseBytes == 0 {
		return response, nil
	}
	if _, err := io.ReadFull(connection, response); err != nil {
		return response, err
	}
	return response, nil
}

// tlsCertificates performs a bounded handshake while deliberately deferring
// peer verification until after the certificate chain is captured. This lets
// expiry and hostname probes describe an invalid leaf instead of reducing it
// to an opaque handshake error. No application bytes are sent.
func (self *runner) tlsCertificates(ctx context.Context, network, address, serverName string) (TLSCertificateObservation, error) {
	timeout := self.cfg.commandTimeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	connection, err := (&tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config: &tls.Config{
			ServerName:         serverName,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // verification follows against the captured chain
		},
	}).DialContext(commandCtx, network, address)
	if err != nil {
		return TLSCertificateObservation{}, err
	}
	defer connection.Close()

	tlsConnection, ok := connection.(*tls.Conn)
	if !ok {
		return TLSCertificateObservation{}, fmt.Errorf("TLS dial returned %T", connection)
	}
	peer := tlsConnection.ConnectionState().PeerCertificates
	if len(peer) == 0 {
		return TLSCertificateObservation{}, fmt.Errorf("TLS peer returned no certificate")
	}
	observation := TLSCertificateObservation{Certificates: make([][]byte, 0, len(peer))}
	for _, certificate := range peer {
		observation.Certificates = append(observation.Certificates, append([]byte(nil), certificate.Raw...))
	}

	intermediates := x509.NewCertPool()
	for _, certificate := range peer[1:] {
		intermediates.AddCert(certificate)
	}
	_, observation.VerifyError = peer[0].Verify(x509.VerifyOptions{
		DNSName:       serverName,
		Intermediates: intermediates,
		CurrentTime:   time.Now(),
	})
	return observation, nil
}

// warpctl runs warpctl locally on the monitor machine (assumed present, like
// by-ip/by-pass). Used for fleet-wide log reads (`warpctl logs`) and the
// publish side of the deploy clock (`warpctl ls versions`).
func (self *runner) warpctl(ctx context.Context, args ...string) (string, error) {
	return self.local(ctx, "warpctl", args...)
}

// warpctlStream starts a long-running warpctl command (e.g. `logs ... -f`) and
// returns the running cmd plus a pipe carrying stdout. warpctl writes its own
// transport/retry diagnostics to stderr; those remain monitor diagnostics and
// must never be classified as lines emitted by the remote service. The caller
// owns reading the pipe and waiting on the cmd; ctx cancellation kills the
// process, which closes the pipe and unblocks the reader.
func (self *runner) warpctlStream(ctx context.Context, diagnostics io.Writer, args ...string) (*exec.Cmd, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "warpctl", args...)
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = os.Stderr
	if diagnostics != nil {
		// Preserve local operator diagnostics while giving the standing monitor
		// a separate, explicitly parsed self-health channel. They must never be
		// folded into the requested service's stdout log stream.
		cmd.Stderr = io.MultiWriter(os.Stderr, diagnostics)
	}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, nil, err
	}
	// the child holds its own dup of pw; closing the parent's copy makes the
	// reader see eof when the child exits
	pw.Close()
	return cmd, pr, nil
}
