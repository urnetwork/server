// A deployment prepares the earning boundary once. Worker reads never enroll
// missing authority or change the asset of retained contracts after a hot edit.
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const providerPayoutBoundarySchemaSql = `
CREATE TABLE provider_payout_boundary (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    earning_identity text NOT NULL CHECK (octet_length(earning_identity) BETWEEN 1 AND 4096),
    identity_sha256 varchar(64) NOT NULL CHECK (identity_sha256 ~ '^[0-9a-f]{64}$'),
    initial_config_sha256 varchar(64) NOT NULL CHECK (initial_config_sha256 ~ '^[0-9a-f]{64}$'),
    prepared_at timestamp NOT NULL DEFAULT timezone('utc', now())
);
CREATE FUNCTION guard_provider_payout_boundary() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'provider earning boundary is immutable; retained usage cannot change asset';
END $$;
CREATE TRIGGER provider_payout_boundary_guard BEFORE UPDATE OR DELETE ON provider_payout_boundary
    FOR EACH ROW EXECUTE FUNCTION guard_provider_payout_boundary();
CREATE TRIGGER provider_payout_boundary_truncate_guard BEFORE TRUNCATE ON provider_payout_boundary
    FOR EACH STATEMENT EXECUTE FUNCTION guard_provider_payout_boundary();
`

var ErrProviderEarningBoundaryUnprepared = errors.New("provider earning boundary is unprepared; new allocations and submissions remain held")
var ErrProviderEarningBoundaryMismatch = errors.New("provider earning boundary differs from its retained deployment authority")
var ErrProviderEarningBoundaryUnavailable = errors.New("provider earning boundary observation is unavailable; retained work may retry")
var ErrProviderEarningBoundarySchema = errors.New("provider earning boundary migration or immutable guard is absent/changed")

// A caller canceled its observation; a successful observation of conflicting
// authority is also a hard refusal. Neither is turned into retry by a joined
// timeout. Only an unavailable observation with no such cause permits retry.
func ProviderEarningBoundaryRetryable(ctx context.Context, err error) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	causes := InspectErrorCauses(err)
	if !causes.Complete {
		return false
	}
	unavailable := false
	for _, node := range causes.Nodes {
		if node.Err == context.Canceled || providerBoundaryHardCause(node.Err) != nil {
			return false
		}
		unavailable = unavailable || node.Err == ErrProviderEarningBoundaryUnavailable
	}
	return unavailable
}

// These are positive, typed observations. Truncation or unknown causes must
// never be converted to a schema/authority mismatch.
func providerBoundaryHardCause(err error) error {
	if err == ErrProviderEarningBoundarySchema || err == ErrProviderEarningBoundaryMismatch || err == ErrProviderEarningBoundaryUnprepared {
		return err
	}
	if value, ok := err.(*pgconn.PgError); ok && value != nil && strings.HasPrefix(value.Code, "42") {
		return ErrProviderEarningBoundarySchema
	}
	return nil
}

// Missing/incompatible SQL objects are observed deployment damage. Walk joined
// causes so a simultaneous timeout cannot turn that refusal into an outage.
func providerBoundaryObservationError(err error) error {
	if err == nil {
		return nil
	}
	causes := InspectErrorCauses(err)
	for _, node := range causes.Nodes {
		if hard := providerBoundaryHardCause(node.Err); hard != nil {
			// Retain the original error and expose the observed hard sentinel
			// before its possibly incomplete tree. No custom Is method is run.
			return errors.Join(hard, err)
		}
	}
	// An incomplete cause census is an unavailable observation. The retry
	// admission above withholds a new attempt until its complete cause is known.
	return errors.Join(ErrProviderEarningBoundaryUnavailable, err)
}

// Readiness can advance from blocked to reviewed without changing these earning
// semantics. The initial full config digest is retained separately for audit.
type providerEarningIdentity struct {
	Schema      string `json:"schema"`
	CutoffUtc   string `json:"cutoff_utc"`
	Attribution string `json:"attribution"`
	LegacyUsdc  string `json:"legacy_usdc"`
	Profile     string `json:"profile"`
	ChainId     uint64 `json:"chain_id"`
	GenesisHash string `json:"genesis_hash"`
	Netuid      uint16 `json:"netuid"`
}

type ProviderEarningBoundary struct {
	IdentitySha256      string    `json:"identity_sha256"`
	InitialConfigSha256 string    `json:"initial_config_sha256"`
	CutoffUtc           string    `json:"cutoff_utc"`
	PreparedAt          time.Time `json:"prepared_at"`
	earningIdentity     string
}

func providerPayoutEarningIdentity(policy *ProviderPayoutTransition) (string, string, error) {
	if policy == nil || policy.Cutoff.IsZero() {
		return "", "", ErrProviderEarningBoundaryUnprepared
	}
	identity := providerEarningIdentity{Schema: policy.Schema, CutoffUtc: policy.Cutoff.UTC().Format(time.RFC3339Nano),
		Attribution: policy.Attribution, LegacyUsdc: policy.LegacyUsdc, Profile: policy.Mainnet.Profile,
		ChainId: policy.Mainnet.ChainId, GenesisHash: strings.ToLower(policy.Mainnet.GenesisHash), Netuid: policy.Mainnet.Netuid}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(data)
	return string(data), hex.EncodeToString(digest[:]), nil
}

type providerBoundaryQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Require the actual consumed migration/guards, not equality with the global DB
// version. Unrelated later migrations cannot invalidate a compatible binding.
func requireProviderPayoutBoundarySchema(ctx context.Context, query providerBoundaryQuery, declared bool) (bool, error) {
	index := -1
	for i, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == providerPayoutBoundarySchemaSql {
			index = i
			break
		}
	}
	if index < 0 {
		return false, ErrProviderEarningBoundarySchema
	}
	identity, err := MigrationIdentity(index)
	if err != nil {
		return false, errors.Join(ErrProviderEarningBoundarySchema, err)
	}
	body := strings.Split(providerPayoutBoundarySchemaSql, "$$")[1]
	var installed, tablePresent, guarded bool
	err = query.QueryRow(ctx, `SELECT
        EXISTS(SELECT 1 FROM migration_catalog WHERE migration_index=$1 AND identity_sha256=$2),
        to_regclass('public.provider_payout_boundary') IS NOT NULL,
        EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid
            WHERE t.tgrelid=to_regclass('public.provider_payout_boundary') AND t.tgname='provider_payout_boundary_guard'
            AND t.tgenabled IN ('O','A') AND t.tgtype=27 AND t.tgqual IS NULL AND p.prosrc=$3)
        AND EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid
            WHERE t.tgrelid=to_regclass('public.provider_payout_boundary') AND t.tgname='provider_payout_boundary_truncate_guard'
            AND t.tgenabled IN ('O','A') AND t.tgtype=34 AND t.tgqual IS NULL AND p.prosrc=$3)`, index, identity, body).Scan(&installed, &tablePresent, &guarded)
	if err != nil {
		return false, providerBoundaryObservationError(err)
	}
	if !installed && !tablePresent && !declared {
		return false, nil
	}
	if !installed || !tablePresent || !guarded {
		return false, ErrProviderEarningBoundarySchema
	}
	return true, nil
}

func readProviderPayoutBoundary(ctx context.Context, query providerBoundaryQuery) (*ProviderEarningBoundary, error) {
	binding := &ProviderEarningBoundary{}
	err := query.QueryRow(ctx, `SELECT earning_identity, identity_sha256, initial_config_sha256, prepared_at FROM provider_payout_boundary WHERE singleton`).Scan(
		&binding.earningIdentity, &binding.IdentitySha256, &binding.InitialConfigSha256, &binding.PreparedAt)
	if err != nil {
		causes := InspectErrorCauses(err)
		absent := causes.Complete
		noRowsOrigins := make([]bool, len(causes.Nodes))
		for index, node := range causes.Nodes {
			noRowsOrigins[index] = node.Err == pgx.ErrNoRows
			if node.Parent >= 0 {
				noRowsOrigins[index] = noRowsOrigins[index] || noRowsOrigins[node.Parent]
			}
			// Pgx's exact sentinel unwraps to sql.ErrNoRows. A bare SQL
			// sentinel or another failed sibling is not this driver's absence.
			if node.Leaf && (!noRowsOrigins[index] || node.Err != pgx.ErrNoRows && node.Err != sql.ErrNoRows) {
				absent = false
			}
		}
		if absent {
			return nil, nil
		}
		return nil, providerBoundaryObservationError(err)
	}
	digest := sha256.Sum256([]byte(binding.earningIdentity))
	var identity providerEarningIdentity
	if len(binding.earningIdentity) > 4096 || hex.EncodeToString(digest[:]) != binding.IdentitySha256 || json.Unmarshal([]byte(binding.earningIdentity), &identity) != nil {
		return nil, ErrProviderEarningBoundaryMismatch
	}
	binding.CutoffUtc = identity.CutoffUtc
	return binding, nil
}

// Db/Tx retain their historical panic contract on connection errors. This
// admission converts only error panics to unavailable observations, preserving
// the underlying cause; caller/programming panics still propagate.
func recoverProviderBoundaryObservation(ctx context.Context, returnErr *error) {
	if value := recover(); value != nil {
		if _, ok := value.(runtime.Error); ok {
			panic(value)
		}
		if err, ok := value.(error); ok {
			if ctx != nil && ctx.Err() != nil {
				err = errors.Join(err, ctx.Err())
			}
			*returnErr = providerBoundaryObservationError(err)
			return
		}
		panic(value)
	}
}

// Called explicitly by db migrate --sn-schedule-sha256. Same-boundary replay is
// idempotent; INSERT ON CONFLICT never updates an existing earning generation.
func PrepareProviderPayoutBoundary(ctx context.Context, expectedConfigSha256 string) (binding *ProviderEarningBoundary, returnErr error) {
	defer recoverProviderBoundaryObservation(ctx, &returnErr)
	policy, err := LoadProviderPayoutTransition(ctx)
	if err != nil {
		return nil, err
	}
	return prepareProviderPayoutBoundary(ctx, policy, expectedConfigSha256)
}

func prepareProviderPayoutBoundary(ctx context.Context, policy *ProviderPayoutTransition, expectedConfigSha256 string) (binding *ProviderEarningBoundary, returnErr error) {
	defer recoverProviderBoundaryObservation(ctx, &returnErr)
	if ctx == nil {
		return nil, errors.New("provider boundary preparation requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if policy == nil || !payoutHex(expectedConfigSha256, 32, false) || policy.ConfigSha256 != expectedConfigSha256 {
		return nil, errors.New("provider boundary preparation requires the exact reviewed sn.yml digest")
	}
	identity, digest, err := providerPayoutEarningIdentity(policy)
	if err != nil {
		return nil, err
	}
	Tx(ctx, func(tx PgTx) {
		if _, err := requireProviderPayoutBoundarySchema(ctx, tx, true); err != nil {
			returnErr = err
			return
		}
		_, err := tx.Exec(ctx, `INSERT INTO provider_payout_boundary(singleton,earning_identity,identity_sha256,initial_config_sha256)
            VALUES(true,$1,$2,$3) ON CONFLICT(singleton) DO NOTHING`, identity, digest, expectedConfigSha256)
		if err != nil {
			Raise(providerBoundaryObservationError(err))
		}
		binding, returnErr = readProviderPayoutBoundary(ctx, tx)
		if returnErr == nil && (binding == nil || binding.earningIdentity != identity || binding.IdentitySha256 != digest) {
			returnErr = ErrProviderEarningBoundaryMismatch
		}
	}, TxReadCommitted)
	if returnErr != nil {
		return nil, returnErr
	}
	return binding, nil
}

// No worker path prepares a missing binding. Configuration removal also refuses
// fresh legacy allocation when this database already holds an earning boundary.
func RequireProviderPayoutBoundary(ctx context.Context, policy *ProviderPayoutTransition) (binding *ProviderEarningBoundary, returnErr error) {
	defer recoverProviderBoundaryObservation(ctx, &returnErr)
	if ctx == nil {
		return nil, errors.New("provider earning boundary requires context")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(ErrProviderEarningBoundaryUnavailable, err)
	}
	Db(ctx, func(conn PgConn) {
		present, err := requireProviderPayoutBoundarySchema(ctx, conn, policy != nil)
		if err != nil || !present {
			returnErr = err
			return
		}
		binding, returnErr = readProviderPayoutBoundary(ctx, conn)
		if returnErr != nil {
			return
		}
		if binding == nil {
			if policy != nil {
				returnErr = ErrProviderEarningBoundaryUnprepared
			}
			return
		}
		if policy == nil {
			returnErr = ErrProviderEarningBoundaryMismatch
			return
		}
		identity, digest, err := providerPayoutEarningIdentity(policy)
		if err != nil {
			returnErr = err
			return
		}
		if binding.earningIdentity != identity || binding.IdentitySha256 != digest {
			returnErr = ErrProviderEarningBoundaryMismatch
		}
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return binding, nil
}

// Allocators and earning projections use this entrypoint. A raw status/parser
// read alone never authorizes reinterpreting retained usage or sending money.
func LoadProviderPayoutEarningPolicy(ctx context.Context) (*ProviderPayoutTransition, error) {
	policy, err := LoadProviderPayoutTransition(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := RequireProviderPayoutBoundary(ctx, policy); err != nil {
		return nil, err
	}
	return policy, nil
}
