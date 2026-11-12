package model

// <root>'s budget at each CA on this process's Redis (TLS-M5-16; §13.8's
// Budget; cert-1): {tcb_<ca>_<registered_domain>}, a sorted set of the
// window's new-set orders scored by their admission, checked against the cap
// and added to in one script, in the shape of server/ip_ratelimit.go's sliding
// window, and expiring 7 days after its newest order.
//
// The scripts are the engine's own (certsservice.CertsBudgetSpendScript and
// CertsBudgetSeedScript), run through the host's Redis, so server and the
// fake's Go twins run one source. Each names one key, so every call is one
// slot's. Redis holds soft state only: a key with no marker is rebuilt from
// network_tls_cert (CertsStore.ListNewSetOrders) before anything is spent
// from it, so a lost key never reads as an empty budget.

import (
	"fmt"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
)

// {tcb_<ca>_<registered_domain>}: one key, so one slot, per CA and root. Named
// here for OPS (D-OPS-16) and for the test that loses it.
func TlsCertBudgetKey(ca certsservice.CertsCa, registeredDomain string) string {
	return fmt.Sprintf("{tcb_%s_%s}", ca, registeredDomain)
}

// certs' budget over redis and this process's rows. settings are the engine's,
// as certs.yml laid them over the defaults.
func NewCertsBudget(redis sidecar.Redis, store *CertsStore, settings *certsservice.CertsSettings) *certsservice.CertsBudget {
	if store == nil {
		store = NewCertsStore(nil)
	}
	return certsservice.NewCertsBudget(redis, store, settings)
}
