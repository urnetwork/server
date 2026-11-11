package controller

// sn_ctf_api_pool.go — which capture the flag pool the ledger route and the
// claim relay answer for beside the operator's pool (CAPTURETHEFLAG.md §10).

import (
	"github.com/urfoundation/sn/amendment"
)

// stCtfPoolId is the operator's capture the flag pool, 2^32 + n
// (CAPTURETHEFLAG.md §3.1), which the ledger route and the claim relay read
// beside its pool. Until a root of that pool is mirrored from vault2 it holds
// no account, so reading it changes no answer.
func stCtfPoolId(cfg *StConfig) (uint64, bool) {
	if cfg == nil {
		return 0, false
	}
	return amendment.CtfPoolID(cfg.NoId)
}
