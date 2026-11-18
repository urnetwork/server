package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/operator-core/coretest"
	"github.com/urnetwork/sidecar"
)

// Services that stand in for a host's, by method set alone: no test here calls them.
type stubRedis struct{ operator.Redis }
type stubMetrics struct{ operator.Metrics }
type stubPush struct{ operator.Push }

// Answers a fixed Principal for any token.
type stubAuthenticator struct {
	principal *operator.Principal
}

func (self *stubAuthenticator) Authenticate(ctx context.Context, audience string, jwt string) (*operator.Principal, error) {
	return self.principal, nil
}

// Every process the server repo launches as a service, with its db grant: §19.4's state column, and the
// platform's own services, which hold the platform's Postgres role.
func testProcessDbGrants() map[string]bool {
	return map[string]bool{
		"api": true, "taskworker": true, "connect": true, "gossip": true, "mcp": true, "monitor": true,
		"proxy": true, "competitionworker": true,
		"all": true, "alt": true,
		"dns": true, "mxedge": true, "mxedge-platform": true, "smtpedge": true,
		"evs": false, "gkm": false, "sockets": false, "tlsedge": false, "dnsedge": false,
	}
}

func TestProcessGrantsKeepEvsAndGkmOffTheDb(t *testing.T) {
	for process, db := range testProcessDbGrants() {
		grants, err := ProcessGrants(process)
		if err != nil {
			t.Fatalf("%s: %v", process, err)
		}
		if grants.Db != db {
			t.Errorf("%s: db %v, want %v", process, grants.Db, db)
		}
	}
	for _, unnamed := range []string{"", "smtpedge-next", "bringyourctl", "sidecarctl", "Api"} {
		if _, err := ProcessGrants(unnamed); err == nil {
			t.Errorf("%q: a process the table does not name got grants", unnamed)
		}
	}
}

// Every service image server/cli builds, a directory there holding a Dockerfile, is a process
// ProcessGrants names, as are all and monitor, which run with no image of their own; so a new block
// states its grants before it ships. Each of them whose main is in its directory sets the process's Env
// through SetProcessEnv; dnsedge and mxedge-platform build dns's and mxedge's mains in their other role.
func TestProcessGrantsNameEveryLaunchedProcess(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "cli"))
	if err != nil {
		t.Fatal(err)
	}
	launched := map[string]bool{"all": true, "monitor": true}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "cli", entry.Name(), "Dockerfile")); err == nil {
			launched[entry.Name()] = true
		}
	}
	if len(launched) < 19 {
		t.Fatalf("found %d launched processes under server/cli, fewer than the 19 known: %v", len(launched), launched)
	}
	expected := testProcessDbGrants()
	otherRoleOf := map[string]string{"dnsedge": "dns", "mxedge-platform": "mxedge"}
	for process := range launched {
		if _, err := ProcessGrants(process); err != nil {
			t.Errorf("%s is launched and has no grants: %v", process, err)
		}
		if _, ok := expected[process]; !ok {
			t.Errorf("%s is launched and this test's table does not hold its db grant", process)
		}
		mainPath := filepath.Join("..", "cli", process, "main.go")
		source, err := os.ReadFile(mainPath)
		if err != nil {
			other, ok := otherRoleOf[process]
			if !ok {
				t.Errorf("%s: no main to read: %v", process, err)
				continue
			}
			source, err = os.ReadFile(filepath.Join("..", "cli", other, "main.go"))
			if err != nil {
				t.Errorf("%s: no main of %s to read: %v", process, other, err)
				continue
			}
			if !strings.Contains(string(source), strconv.Quote(process)) {
				t.Errorf("%s: %s's main does not name it as a process", process, other)
			}
		}
		if !strings.Contains(string(source), "core.SetProcessEnv(") {
			t.Errorf("%s: its main does not set the process's Env", process)
		}
	}
	for process := range expected {
		if !launched[process] {
			t.Errorf("%s has grants and server/cli launches no such process", process)
		}
	}
}

// SetProcessEnv builds the process's Env under its grants and sets it as operator's global Env; a
// process the table does not name, or no host, sets nothing and leaves the global Env as it stood.
func TestSetProcessEnvSetsTheGlobalEnv(t *testing.T) {
	coretest.SetGlobalEnv(t, nil)
	unset := operator.GlobalEnv()
	if _, err := SetProcessEnv("smtpedge-next", &sidecar.ServiceHost{}); err == nil {
		t.Errorf("a process the table does not name set an Env")
	}
	if _, err := SetProcessEnv("api", nil); err == nil {
		t.Errorf("no host set an Env")
	}
	if operator.GlobalEnv() != unset {
		t.Fatalf("a refused set changed the global Env")
	}
	coretest.EnvSuite(t, operator.GlobalEnv())

	evs, err := SetProcessEnv("evs", &sidecar.ServiceHost{Redis: &stubRedis{}})
	if err != nil {
		t.Fatal(err)
	}
	if operator.GlobalEnv() != operator.Env(evs) {
		t.Fatalf("the global Env is not the Env SetProcessEnv built")
	}
	coretest.EnvSuite(t, operator.GlobalEnv(), "Redis", "Clock", "Gates", "Abuse")

	api, err := SetProcessEnv("api", &sidecar.ServiceHost{Metrics: &stubMetrics{}})
	if err != nil {
		t.Fatal(err)
	}
	if operator.GlobalEnv() != operator.Env(api) {
		t.Fatalf("a later process Env did not replace the earlier one")
	}
	coretest.EnvSuite(t, operator.GlobalEnv(), "Db", "Metrics", "Clock", "Gates", "Abuse")

	if _, err := SetProcessEnv("unknown", &sidecar.ServiceHost{}); err == nil {
		t.Errorf("an unknown process set an Env")
	}
	if operator.GlobalEnv() != operator.Env(api) {
		t.Errorf("a refused set replaced the Env that stood")
	}
}

// A block that could not start sets its Env over an empty host: every service refuses but the ones
// ServiceHost answers closed, and the db where the process's grants hold it.
func TestSetProcessEnvOverAnEmptyHostRefuses(t *testing.T) {
	coretest.SetGlobalEnv(t, nil)
	if _, err := SetProcessEnv("sockets", &sidecar.ServiceHost{}); err != nil {
		t.Fatal(err)
	}
	coretest.EnvSuite(t, operator.GlobalEnv(), "Clock", "Gates", "Abuse")
	if _, err := SetProcessEnv("smtpedge", &sidecar.ServiceHost{}); err != nil {
		t.Fatal(err)
	}
	coretest.EnvSuite(t, operator.GlobalEnv(), "Db", "Clock", "Gates", "Abuse")
	gates, err := operator.GlobalEnv().Gates()
	if err != nil || gates.Service(sidecar.ServiceGateEvs).Enabled() {
		t.Errorf("an empty host opened a gate: %v", err)
	}
}

func TestNewEnvRefusesNoHostOrGrants(t *testing.T) {
	if _, err := NewEnv(nil, &Grants{}); err == nil {
		t.Errorf("an Env with no host")
	}
	if _, err := NewEnv(&sidecar.ServiceHost{}, nil); err == nil {
		t.Errorf("an Env with no grants")
	}
	if _, err := NewProcessEnv("unknown", &sidecar.ServiceHost{}); err == nil {
		t.Errorf("an Env for an unknown process")
	}
}

// What the host was given is granted, what it was not is refused by name, and the clock, gates and abuse
// gate keep ServiceHost's fail-closed answers for a host that set none.
func TestEnvGrantsWhatTheHostHolds(t *testing.T) {
	host := &sidecar.ServiceHost{
		Redis:   &stubRedis{},
		Metrics: &stubMetrics{},
		Push:    &stubPush{},
	}
	env, err := NewProcessEnv("api", host)
	if err != nil {
		t.Fatal(err)
	}
	coretest.EnvSuite(t, env, "Db", "Redis", "Metrics", "Push", "Clock", "Gates", "Abuse")
	gates, err := env.Gates()
	if err != nil || gates.Service(sidecar.ServiceGateEvs).Enabled() {
		t.Errorf("a host with no gates opened one: %v", err)
	}
	abuse, err := env.Abuse()
	if err != nil {
		t.Fatal(err)
	}
	if _, loaded := abuse.Standing(connect.NewId()); loaded {
		t.Errorf("a host with no abuse gate loaded a network")
	}
}

// evs keeps its durable state in the file clusters alone, so its Env refuses the db however much else
// its host holds.
func TestEnvRefusesTheDbToEvs(t *testing.T) {
	env, err := NewProcessEnv("evs", &sidecar.ServiceHost{Redis: &stubRedis{}})
	if err != nil {
		t.Fatal(err)
	}
	coretest.EnvSuite(t, env, "Redis", "Clock", "Gates", "Abuse")
	if _, err := env.Db(); !errors.Is(err, operator.ErrNotGranted) {
		t.Errorf("evs got a db: %v", err)
	}
}

// The Env's Authenticator is ServiceHost.Authenticate, so what Principal.Validate refuses, or a Principal
// for another audience, never reaches an extension, whatever the host's Authenticator answers.
func TestEnvAuthenticatorKeepsServiceHostsChecks(t *testing.T) {
	valid := &operator.Principal{NetworkId: connect.NewId(), ClientId: connect.NewId(), Audience: "evs"}
	for _, c := range []struct {
		principal *operator.Principal
		audience  string
		refused   bool
	}{
		{principal: valid, audience: "evs", refused: false},
		{principal: valid, audience: "gkm", refused: true},
		{principal: &operator.Principal{ClientId: connect.NewId(), Audience: "evs"}, audience: "evs", refused: true},
		{principal: valid, audience: "", refused: true},
	} {
		env, err := NewProcessEnv("evs", &sidecar.ServiceHost{Authenticator: &stubAuthenticator{principal: c.principal}})
		if err != nil {
			t.Fatal(err)
		}
		authenticator, err := env.Authenticator()
		if err != nil {
			t.Fatal(err)
		}
		principal, err := authenticator.Authenticate(context.Background(), c.audience, "a token")
		if c.refused {
			if !errors.Is(err, operator.ErrAuthenticationRefused) || principal != nil {
				t.Errorf("%+v for %q: %v, %+v", c.principal, c.audience, err, principal)
			}
			continue
		}
		if err != nil || principal != c.principal {
			t.Errorf("%+v for %q: %v", c.principal, c.audience, err)
		}
	}
}
