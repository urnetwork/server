package core

import (
	"context"
	"errors"
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

func TestProcessGrantsKeepEvsAndGkmOffTheDb(t *testing.T) {
	for process, db := range map[string]bool{
		"api": true, "taskworker": true, "dns": true, "mxedge": true,
		"evs": false, "gkm": false, "sockets": false, "tlsedge": false, "dnsedge": false,
	} {
		grants, err := ProcessGrants(process)
		if err != nil {
			t.Fatalf("%s: %v", process, err)
		}
		if grants.Db != db {
			t.Errorf("%s: db %v, want %v", process, grants.Db, db)
		}
	}
	if _, err := ProcessGrants("smtpedge-next"); err == nil {
		t.Errorf("a process the table does not name got grants")
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
