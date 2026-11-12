package controller

// sn_ctf_api_pool.go — which capture the flag pool the ledger route and the
// claim relay answer for beside the operator's pool (CAPTURETHEFLAG.md §10),
// and the switch that has them read it.

import (
	"github.com/urfoundation/sn/amendment"
)

// stCtfPoolId is the operator's capture the flag pool, 2^32 + n
// (CAPTURETHEFLAG.md §3.1), which the ledger route and the claim relay read
// beside its pool while capture the flag is on (stCtfPoolOn). Until a root of
// that pool is mirrored from vault2 it holds no account, so reading it changes
// no answer.
func stCtfPoolId(cfg *StConfig) (uint64, bool) {
	if cfg == nil {
		return 0, false
	}
	return amendment.CtfPoolID(cfg.NoId)
}

// stCtfPoolOn reports whether the ledger route and the claim relay read
// capture the flag pools: st.yml's ctf block runs capture the flag (shadow or
// live) and ctf.yml's api block enables it. While either is off they read
// none, not even one the account index records a coldkey in, so no fault of a
// capture the flag pool can reach a client; the pool's accounts are answered
// and claimed again once both are on.
func stCtfPoolOn(cfg *StConfig) bool {
	return cfg != nil && cfg.Ctf.On() && snCtfApiConfigOf().Enabled
}
