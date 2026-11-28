// Release receipts describe the existing actual pool generation. They do not
// replace its destructor or claim that an asynchronously closed socket joined.
package server

import "github.com/jackc/pgx/v5/pgxpool"

// The configured hooks run outside pgxpool's bounded Close/cleanup wait. An
// unknown hook therefore cannot acquire the fixed no-hook disposal contract.
// The production constructor does not retain/mutate its config after creation.
func dbPoolReleasePolicyKnown(config *pgxpool.Config) bool {
	if config == nil || config.ConnConfig == nil || config.MaxConns <= 0 || config.BeforeClose != nil || config.AfterRelease != nil {
		return false
	}
	_, releaseTracer := config.ConnConfig.Tracer.(pgxpool.ReleaseTracer)
	return !releaseTracer
}
