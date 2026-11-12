package sidecart3

// §22.8's disruptions beyond the schedules (TEST-M2-15; D-TEST-7, R-TEST-2): what happens to T3's real processes and
// stores while a sidecar works, carried out for real rather than modelled, since what each tests is the real thing's own
// behaviour. Each is a Disruption a row's test calls at the moment it chooses:
//
//   - RedisRestart, a Redis master's restart: every connection to the test's database drops and every key in it goes,
//     since production's cluster runs no replicas (xops/main/ansible/REDIS.md:3), so its soft state is lost, not
//     failed over. It is scoped to the test's own database on the private test Redis, which other tracks' tests share,
//     so the container itself never restarts.
//   - PostgresRestart, a Postgres or PgBouncer restart: every server connection of the test's own database but the one
//     asking drops, mid-transaction where one is open, as a restart's does; other databases on the shared server are
//     not touched.
//   - ResidentKill, a resident's death on server's exchange, as its host's crash or a move's retirement ends it: the
//     client's next transport nominates a new one.
//   - LbFixture.Drain, an lb drain: nginx, from warp's generated directives (local/sidecar-lb-fixture), in front of
//     upstream blocks, one of which goes away with its connections cut, as warp kills a block's old container.
//
// These use real time and run in sidecar-t3.sh's nightly form under the sidecar_disruption tag, never at merge. Sockets
// healing and halts reloading join at M5 and M9 with their owners. Safe for concurrent use unless a type says otherwise.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
)

// One of §22.8's disruptions, carried out at a moment its test chooses.
type Disruption interface {
	// "redis master restart", for a failure's message.
	String() string
	// Carries the disruption out, returning once it has taken effect.
	Disrupt(ctx context.Context) error
}

// A Redis master's restart, on the test's own database of the private test Redis: every connection to that database
// is killed and every key in it removed.
type RedisRestart struct {
	stateLock   sync.Mutex
	killedCount int
}

func NewRedisRestart() *RedisRestart {
	return &RedisRestart{}
}

// How many connections the restarts so far cut, the asking one aside: none means the restart left every pool's
// connection standing, which a real one does not.
func (self *RedisRestart) KilledCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.killedCount
}

func (self *RedisRestart) String() string {
	return "redis master restart"
}

// Kills every client connection on the test's database, server's pools' among them, the asking one aside, and empties
// the database. A pool redials on its next command, as after a restart. Refuses a Redis that is not one node with
// databases, where a test's own database cannot be told from another's.
func (self *RedisRestart) Disrupt(ctx context.Context) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("redis master restart: %v", recovered)
		}
	}()
	server.Redis(ctx, func(client server.RedisClient) {
		node, ok := client.(*redis.Client)
		if !ok {
			returnErr = fmt.Errorf("redis master restart: server's Redis is a %T, not one node a test's database can be told on", client)
			return
		}
		database := node.Options().DB
		list, err := node.ClientList(ctx).Result()
		if err != nil {
			returnErr = err
			return
		}
		clientIds, err := redisClientIdsOnDatabase(list, database)
		if err != nil {
			returnErr = err
			return
		}
		killedCount := 0
		for _, clientId := range clientIds {
			// CLIENT KILL with a filter skips the connection asking, and answers how many it killed
			killed, err := node.Do(ctx, "CLIENT", "KILL", "ID", strconv.FormatInt(clientId, 10)).Int()
			if err != nil {
				returnErr = err
				return
			}
			killedCount += killed
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.killedCount += killedCount
		}()
		returnErr = node.FlushDB(ctx).Err()
	})
	return returnErr
}

// The ids of the connections CLIENT LIST shows on database.
func redisClientIdsOnDatabase(list string, database int) ([]int64, error) {
	clientIds := []int64{}
	scanner := bufio.NewScanner(strings.NewReader(list))
	for scanner.Scan() {
		fields := map[string]string{}
		for _, field := range strings.Fields(scanner.Text()) {
			if key, value, ok := strings.Cut(field, "="); ok {
				fields[key] = value
			}
		}
		if fields["db"] != strconv.Itoa(database) {
			continue
		}
		clientId, err := strconv.ParseInt(fields["id"], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("a CLIENT LIST line with no id: %w", err)
		}
		clientIds = append(clientIds, clientId)
	}
	return clientIds, scanner.Err()
}

// What a Postgres restart runs with.
type PostgresRestartSettings struct {
	// How long each backend's termination may take to show, as pg_terminate_backend waits for it: past this the
	// restart fails, since a backend that outlived it did not go down.
	TerminateTimeout time.Duration
}

func DefaultPostgresRestartSettings() *PostgresRestartSettings {
	return &PostgresRestartSettings{
		TerminateTimeout: 5 * time.Second,
	}
}

// A Postgres or PgBouncer restart, on the test's own database: every backend on it but the asking one is terminated.
type PostgresRestart struct {
	settings *PostgresRestartSettings
}

// settings nil takes the defaults.
func NewPostgresRestart(settings *PostgresRestartSettings) *PostgresRestart {
	if settings == nil {
		settings = DefaultPostgresRestartSettings()
	}
	return &PostgresRestart{settings: settings}
}

func (self *PostgresRestart) String() string {
	return "postgres restart"
}

// Terminates every other backend of the current database and answers once each is gone. A transaction open on one rolls
// back, and its pool connection fails at its next use.
func (self *PostgresRestart) Disrupt(ctx context.Context) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("postgres restart: %v", recovered)
		}
	}()
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT pg_terminate_backend(pid, $1) FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type = 'client backend'`,
			self.settings.TerminateTimeout.Milliseconds(),
		)
		if err != nil {
			returnErr = err
			return
		}
		defer rows.Close()
		for rows.Next() {
			terminated := false
			if err := rows.Scan(&terminated); err != nil {
				returnErr = err
				return
			}
			if !terminated {
				returnErr = fmt.Errorf("postgres restart: a backend outlived its termination's %s", self.settings.TerminateTimeout)
				return
			}
		}
		returnErr = rows.Err()
	})
	return returnErr
}

// A resident's death on server's exchange: each client's resident there is cancelled.
type ResidentKill struct {
	exchange  *connectserver.Exchange
	clientIds []connect.Id
}

func NewResidentKill(exchange *connectserver.Exchange, clientIds ...connect.Id) *ResidentKill {
	return &ResidentKill{exchange: exchange, clientIds: clientIds}
}

func (self *ResidentKill) String() string {
	return fmt.Sprintf("resident kill of %d clients", len(self.clientIds))
}

// Cancels each client's resident. A client with none fails it: a disruption that changes nothing proves nothing.
func (self *ResidentKill) Disrupt(ctx context.Context) error {
	for _, clientId := range self.clientIds {
		if !self.exchange.Testing_CancelLocalResident(server.Id(clientId)) {
			return fmt.Errorf("resident kill: client %s has no resident on the exchange", clientId)
		}
	}
	return nil
}

// What the lb fixture runs with.
type LbFixtureSettings struct {
	// The nginx to run: SIDECAR_LB_NGINX when set, else nginx on PATH.
	NginxBinary string
	// The fixture's template, from this package's directory, which go test runs in.
	ConfigPath string
	// How long, in real time, nginx may take to listen once started, and how often the start looks meanwhile.
	StartTimeout      time.Duration
	StartPollInterval time.Duration
	// How long, in real time, nginx and an upstream may take to stop.
	StopTimeout time.Duration
}

func DefaultLbFixtureSettings() *LbFixtureSettings {
	nginxBinary := os.Getenv("SIDECAR_LB_NGINX")
	if nginxBinary == "" {
		nginxBinary = "nginx"
	}
	return &LbFixtureSettings{
		NginxBinary:       nginxBinary,
		ConfigPath:        "../local/sidecar-lb-fixture/nginx.conf",
		StartTimeout:      10 * time.Second,
		StartPollInterval: 20 * time.Millisecond,
		StopTimeout:       10 * time.Second,
	}
}

// One upstream block behind the lb: a handler on a loopback listener of its own.
type LbUpstream struct {
	Name    string
	Handler http.Handler

	listener net.Listener
	server   *http.Server
	drained  bool
}

// The address warp's upstream names for it.
func (self *LbUpstream) Address() string {
	return self.listener.Addr().String()
}

// nginx on local/sidecar-lb-fixture's config in front of its upstreams, until Close. Not safe for concurrent use by
// several tests; one fixture's methods are safe for concurrent use.
type LbFixture struct {
	t         testing.TB
	settings  *LbFixtureSettings
	directory string
	port      int
	command   *exec.Cmd
	exited    chan struct{}

	stateLock sync.Mutex
	upstreams []*LbUpstream
}

// Starts each upstream on a loopback listener and nginx in front of them, in upstreams' order, answering once nginx
// listens. Fails when nginx does not run: the row it serves is not run without it. settings nil takes the defaults.
func NewLbFixture(t testing.TB, settings *LbFixtureSettings, upstreams ...*LbUpstream) (*LbFixture, error) {
	if settings == nil {
		settings = DefaultLbFixtureSettings()
	}
	nginxBinary, err := exec.LookPath(settings.NginxBinary)
	if err != nil {
		return nil, fmt.Errorf("the lb fixture needs nginx (%s): %w", settings.NginxBinary, err)
	}
	configTemplate, err := template.ParseFiles(settings.ConfigPath)
	if err != nil {
		return nil, err
	}
	self := &LbFixture{t: t, settings: settings, directory: t.TempDir(), exited: make(chan struct{})}
	addresses := []string{}
	for _, upstream := range upstreams {
		listener, _, err := listenLoopback("tcp4")
		if err != nil {
			self.Close()
			return nil, err
		}
		upstream.listener = listener
		upstream.server = &http.Server{Handler: upstream.Handler}
		go upstream.server.Serve(listener)
		self.upstreams = append(self.upstreams, upstream)
		addresses = append(addresses, upstream.Address())
	}
	// the port nginx takes: one the OS gave a listener a moment ago, which is no data tunnel's
	portListener, port, err := listenLoopback("tcp4")
	if err != nil {
		self.Close()
		return nil, err
	}
	portListener.Close()
	self.port = port
	config := bytes.Buffer{}
	if err := configTemplate.Execute(&config, map[string]any{
		"Directory":  self.directory,
		"ListenPort": port,
		"Upstreams":  addresses,
	}); err != nil {
		self.Close()
		return nil, err
	}
	configPath := filepath.Join(self.directory, "nginx.conf")
	if err := os.WriteFile(configPath, config.Bytes(), 0o600); err != nil {
		self.Close()
		return nil, err
	}
	self.command = exec.Command(nginxBinary, "-g", "daemon off;", "-p", self.directory, "-c", configPath, "-e", filepath.Join(self.directory, "error.log"))
	if err := self.command.Start(); err != nil {
		self.Close()
		return nil, err
	}
	go func() {
		self.command.Wait()
		close(self.exited)
	}()
	if err := self.awaitListening(); err != nil {
		self.Close()
		return nil, err
	}
	return self, nil
}

// Waits for nginx's listener, or its exit, or StartTimeout.
func (self *LbFixture) awaitListening() error {
	deadline := time.After(self.settings.StartTimeout)
	for {
		conn, err := net.DialTimeout("tcp4", self.address(), self.settings.StartPollInterval)
		if err == nil {
			conn.Close()
			return nil
		}
		select {
		case <-self.exited:
			return fmt.Errorf("nginx exited before it listened: %s", self.ErrorLog())
		case <-deadline:
			return fmt.Errorf("nginx did not listen within %s: %s", self.settings.StartTimeout, self.ErrorLog())
		case <-time.After(self.settings.StartPollInterval):
		}
	}
}

func (self *LbFixture) address() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(self.port))
}

// The lb's url, which a client names.
func (self *LbFixture) Url() string {
	return "http://" + self.address()
}

// nginx's error log so far.
func (self *LbFixture) ErrorLog() string {
	log, _ := os.ReadFile(filepath.Join(self.directory, "error.log"))
	return string(log)
}

// The upstream named name, or nil.
func (self *LbFixture) Upstream(name string) *LbUpstream {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, upstream := range self.upstreams {
		if upstream.Name == name {
			return upstream
		}
	}
	return nil
}

// The drain of the upstream named name: its listener closes and every connection it holds is cut, a request mid-way
// through its body included, as warp kills a block's old container (warp/warpctl/run.go's drain).
func (self *LbFixture) Drain(name string) Disruption {
	return &lbDrain{fixture: self, name: name}
}

type lbDrain struct {
	fixture *LbFixture
	name    string
}

func (self *lbDrain) String() string {
	return "lb drain of " + self.name
}

func (self *lbDrain) Disrupt(ctx context.Context) error {
	upstream := self.fixture.Upstream(self.name)
	if upstream == nil {
		return fmt.Errorf("lb drain: no upstream %s", self.name)
	}
	self.fixture.stateLock.Lock()
	drained := upstream.drained
	upstream.drained = true
	self.fixture.stateLock.Unlock()
	if drained {
		return fmt.Errorf("lb drain: %s is drained already", self.name)
	}
	// Close, not Shutdown: a drain's kill does not wait for what is in flight
	return upstream.server.Close()
}

// Stops nginx and every upstream.
func (self *LbFixture) Close() {
	if self.command != nil && self.command.Process != nil {
		self.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-self.exited:
		case <-time.After(self.settings.StopTimeout):
			self.command.Process.Kill()
			<-self.exited
		}
	}
	self.stateLock.Lock()
	upstreams := self.upstreams
	self.stateLock.Unlock()
	for _, upstream := range upstreams {
		if upstream.server != nil {
			upstream.server.Close()
		} else if upstream.listener != nil {
			upstream.listener.Close()
		}
	}
}
