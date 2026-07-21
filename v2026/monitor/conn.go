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
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// address modes select which host address the monitor dials
	addressModeLan     = "lan"     // deployed in-environment
	addressModeOverlay = "overlay" // local dev over the vpn
)

// host is one monitored host from the inventory (vault/<env>/monitor.yml).
type host struct {
	name      string
	lanIp     string // resolved from config settings.yml routes (lan mode)
	overlayIp string // from monitor.yml (overlay mode)
	roles     []string
	// redis-cluster hosts only
	redisEntryPort int
	redisNodeLo    int
	redisNodeHi    int
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
	env string // WARP_ENV

	sshUser     string // deployed login user
	sshDevUser  string // login user for local dev over the overlay
	addressMode string

	hosts []*host

	pgPort        int
	pgbouncerPort int
	pgUser        string
	pgPassword    string
	pgDb          string

	// state dir for baselines and other local persistence
	stateDir string

	// hard timeouts; a command exceeding these is recorded as unreachable
	sshConnectTimeout time.Duration
	commandTimeout    time.Duration
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
	cfg *monitorConfig
}

func newRunner(cfg *monitorConfig) *runner {
	return &runner{cfg: cfg}
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

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := fmt.Sprintf("%s@%s", self.cfg.activeSshUser(), addr)
	cmd := exec.CommandContext(
		cmdCtx,
		"ssh",
		"-o", "BatchMode=yes",
		"-o", fmt.Sprintf("ConnectTimeout=%d", int(connectTimeout.Seconds())),
		"-o", "StrictHostKeyChecking=accept-new",
		target,
		remoteCmd,
	)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	err := cmd.Run()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return out.String(), &unreachableError{host: h.name, err: fmt.Errorf("timeout after %s", timeout)}
	}
	if err != nil {
		// ssh dial failures (exit 255) mean the host itself is unobservable;
		// a nonzero exit from the remote command is a command error. Both
		// surface with the stderr text.
		return out.String(), fmt.Errorf("%s: %w: %s", h.name, err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
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
	remoteCmd := fmt.Sprintf("redis-cli -p %d %s", port, strings.Join(args, " "))
	out, err := self.ssh(ctx, h, remoteCmd, "")
	return strings.TrimSpace(out), err
}

// shell runs an observational shell command on a host (top, ss, dmesg,
// journalctl, docker ps).
func (self *runner) shell(ctx context.Context, h *host, remoteCmd string) (string, error) {
	return self.ssh(ctx, h, remoteCmd, "")
}

// warpctl runs warpctl locally on the monitor machine (assumed present, like
// by-ip/by-pass). Used for fleet-wide log reads (`warpctl logs`) and the
// publish side of the deploy clock (`warpctl ls versions`). Output is the
// combined stdout+stderr — warpctl emits informational lines through Go's
// log package (stderr), and callers parse those too.
func (self *runner) warpctl(ctx context.Context, args ...string) (string, error) {
	timeout := self.cfg.commandTimeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "warpctl", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if cmdCtx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("warpctl timeout after %s", timeout)
	}
	if err != nil {
		return out.String(), fmt.Errorf("warpctl %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}

// warpctlStream starts a long-running warpctl command (e.g. `logs ... -f`) and
// returns the running cmd plus a pipe carrying its combined stdout+stderr.
// The caller owns reading the pipe and waiting on the cmd; ctx cancellation
// kills the process, which closes the pipe and unblocks the reader.
func (self *runner) warpctlStream(ctx context.Context, args ...string) (*exec.Cmd, io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "warpctl", args...)
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
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
