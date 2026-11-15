// Package core is server's implementation of operator-core's host contract (owner decision 251; sidecar's
// DESIGN.md §4.5): an operator.Env built from a process's existing services, with the process's grants,
// and server's pgx Db and Tx helpers adapted to operator.Db (db.go).
//
// The Env wraps the sidecar.ServiceHost that server's host packages assemble (sidecarhost, evshost,
// dnshost, mxedgehost and quotahost), so the processes keep building ServiceHost as before and no
// engine's constructor changes; new code takes the Env. Each accessor answers the host's service, or an
// operator.NotGrantedError when the process's role does not allow the service or the host was given
// none. The clock, the gates and the abuse gate keep ServiceHost's own answers for a host that set none:
// real time, every gate closed, and no network loaded, each of which fails closed.
package core

import (
	"context"
	"fmt"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
)

// What a process's Env may hand out beyond what its ServiceHost holds.
type Grants struct {
	// Whether the process holds a Postgres pool its role lets an extension use, §19.4's state column.
	// evs and gkm never do: they keep durable state only in the file clusters (owner decisions 2, 19,
	// 21).
	Db bool
}

// The grants of each process that hosts an engine, by the service name of its block (§19.4), from what
// each host package builds today: a block whose host holds Postgres is granted the db, and one that
// holds none is not. A process this table does not name is refused, so a new block states its grants
// before it gets an Env.
func ProcessGrants(process string) (*Grants, error) {
	switch process {
	case "api", "taskworker", "dns", "mxedge":
		return &Grants{Db: true}, nil
	case "evs", "gkm", "sockets", "tlsedge", "dnsedge":
		return &Grants{Db: false}, nil
	}
	return nil, fmt.Errorf("no grants for the process %q", process)
}

// One process's Env over its ServiceHost. Safe for concurrent use: it holds no state of its own beyond
// what construction set, and every service it hands out is safe for concurrent use.
type Env struct {
	host   *sidecar.ServiceHost
	grants Grants
	db     operator.Db
}

var _ operator.Env = (*Env)(nil)

// The Env of a process holding host, under grants. The db, when granted, is server's own pools through
// NewDb.
func NewEnv(host *sidecar.ServiceHost, grants *Grants) (*Env, error) {
	if host == nil {
		return nil, fmt.Errorf("an Env needs the process's ServiceHost")
	}
	if grants == nil {
		return nil, fmt.Errorf("an Env needs the process's grants")
	}
	self := &Env{
		host:   host,
		grants: *grants,
	}
	if grants.Db {
		self.db = NewDb()
	}
	return self, nil
}

// NewEnv under ProcessGrants(process).
func NewProcessEnv(process string, host *sidecar.ServiceHost) (*Env, error) {
	grants, err := ProcessGrants(process)
	if err != nil {
		return nil, err
	}
	return NewEnv(host, grants)
}

// The refusal for a service the host was given none of.
func notGiven(service string) error {
	return &operator.NotGrantedError{Service: service, Reason: "the host was given none"}
}

// server's Postgres, where the process's grants allow it.
func (self *Env) Db() (operator.Db, error) {
	if !self.grants.Db {
		return nil, &operator.NotGrantedError{Service: "Db", Reason: "this process's role holds no Postgres"}
	}
	return self.db, nil
}

// The host's Redis.
func (self *Env) Redis() (operator.Redis, error) {
	if self.host.Redis == nil {
		return nil, notGiven("Redis")
	}
	return self.host.Redis, nil
}

// The host's file clusters.
func (self *Env) Blobs() (operator.BlobStore, error) {
	if self.host.Blobs == nil {
		return nil, notGiven("Blobs")
	}
	return self.host.Blobs, nil
}

// The host's Authenticator behind ServiceHost.Authenticate, so a Principal that Validate refuses, or one
// for another audience, never reaches an extension whatever the host's Authenticator answers.
func (self *Env) Authenticator() (operator.Authenticator, error) {
	if self.host.Authenticator == nil {
		return nil, notGiven("Authenticator")
	}
	return &hostAuthenticator{host: self.host}, nil
}

// The host's quota.
func (self *Env) Quota() (operator.Quota, error) {
	if self.host.Quota == nil {
		return nil, notGiven("Quota")
	}
	return self.host.Quota, nil
}

// The host's metrics.
func (self *Env) Metrics() (operator.Metrics, error) {
	if self.host.Metrics == nil {
		return nil, notGiven("Metrics")
	}
	return self.host.Metrics, nil
}

// The host's tasks, which only a process holding Postgres is given.
func (self *Env) Tasks() (operator.Tasks, error) {
	if self.host.Tasks == nil {
		return nil, notGiven("Tasks")
	}
	return self.host.Tasks, nil
}

// The host's push.
func (self *Env) Push() (operator.Push, error) {
	if self.host.Push == nil {
		return nil, notGiven("Push")
	}
	return self.host.Push, nil
}

// The host's source of platform client credentials.
func (self *Env) ServiceClient() (operator.ServiceClientSource, error) {
	if self.host.ServiceClient == nil {
		return nil, notGiven("ServiceClient")
	}
	return self.host.ServiceClient, nil
}

// The host's clock, or real time when it set none.
func (self *Env) Clock() (operator.Clock, error) {
	return self.host.ClockOrReal(), nil
}

// The host's clock check.
func (self *Env) ClockCheck() (operator.ClockCheck, error) {
	if self.host.ClockCheck == nil {
		return nil, notGiven("ClockCheck")
	}
	return self.host.ClockCheck, nil
}

// The host's gates, or every gate closed when it set none.
func (self *Env) Gates() (operator.ServiceGates, error) {
	return self.host.GatesOrClosed(), nil
}

// The host's write budget.
func (self *Env) Budget() (operator.WriteBudget, error) {
	if self.host.Budget == nil {
		return nil, notGiven("Budget")
	}
	return self.host.Budget, nil
}

// The host's abuse gate, or one that has loaded no network when it set none.
func (self *Env) Abuse() (operator.AbuseGate, error) {
	return self.host.AbuseOrUnloaded(), nil
}

// ServiceHost.Authenticate as an operator.Authenticator.
type hostAuthenticator struct {
	host *sidecar.ServiceHost
}

// ServiceHost.Authenticate, Validate and the audience match included.
func (self *hostAuthenticator) Authenticate(ctx context.Context, audience string, jwt string) (*operator.Principal, error) {
	return self.host.Authenticate(ctx, audience, jwt)
}
