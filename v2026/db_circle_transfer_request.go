package server

import (
	"context"
	"errors"
	"strings"
)

// Customer challenge custody is separate from provider payout liabilities.
// Exact requests and normalized observations are retained without user tokens.
const circleTransferRequestSchemaSql = `
CREATE TABLE circle_transfer_request (
 network_id uuid NOT NULL,
 user_id uuid NOT NULL,
 request_id uuid NOT NULL,
 idempotency_key uuid NOT NULL UNIQUE,
 basis text NOT NULL CHECK (octet_length(basis) BETWEEN 1 AND 8192),
 request_body text NOT NULL CHECK (octet_length(request_body) BETWEEN 1 AND 8192),
 challenge_id text NULL CHECK (challenge_id IS NULL OR octet_length(challenge_id) BETWEEN 1 AND 256),
 challenge_status text NOT NULL DEFAULT 'UNKNOWN' CHECK (challenge_status IN ('UNKNOWN','PENDING','IN_PROGRESS','COMPLETE','FAILED','EXPIRED')),
 submission_count bigint NOT NULL DEFAULT 0 CHECK (submission_count>=0),
 review_required boolean NOT NULL DEFAULT false,
 create_time timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(network_id,user_id,request_id)
);
CREATE TABLE circle_transfer_observation (
 network_id uuid NOT NULL,
 user_id uuid NOT NULL,
 request_id uuid NOT NULL,
 digest text NOT NULL CHECK (length(digest)=64),
 detail text NOT NULL CHECK (octet_length(detail) BETWEEN 1 AND 16384),
 create_time timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(network_id,user_id,request_id,digest),
 FOREIGN KEY(network_id,user_id,request_id) REFERENCES circle_transfer_request(network_id,user_id,request_id)
);
CREATE FUNCTION circle_transfer_request_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
 IF TG_OP IN ('DELETE','TRUNCATE') THEN RAISE EXCEPTION 'customer transfer request history is retained'; END IF;
 IF (NEW.network_id,NEW.user_id,NEW.request_id,NEW.idempotency_key,NEW.basis,NEW.request_body,NEW.create_time)
   IS DISTINCT FROM (OLD.network_id,OLD.user_id,OLD.request_id,OLD.idempotency_key,OLD.basis,OLD.request_body,OLD.create_time)
   OR (OLD.challenge_id IS NOT NULL AND NEW.challenge_id IS DISTINCT FROM OLD.challenge_id)
   OR NEW.submission_count<OLD.submission_count
   OR (OLD.review_required AND NOT NEW.review_required)
   OR (OLD.challenge_status IN ('COMPLETE','FAILED','EXPIRED') AND NEW.challenge_status<>OLD.challenge_status)
 THEN RAISE EXCEPTION 'customer transfer request identity is immutable'; END IF;
 RETURN NEW;
END $guard$;
CREATE TRIGGER circle_transfer_request_guard BEFORE UPDATE OR DELETE ON circle_transfer_request
 FOR EACH ROW EXECUTE FUNCTION circle_transfer_request_guard();
CREATE TRIGGER circle_transfer_request_truncate_guard BEFORE TRUNCATE ON circle_transfer_request
 FOR EACH STATEMENT EXECUTE FUNCTION circle_transfer_request_guard();
CREATE FUNCTION circle_transfer_observation_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN RAISE EXCEPTION 'customer transfer observations are append only'; END $guard$;
CREATE TRIGGER circle_transfer_observation_guard BEFORE UPDATE OR DELETE ON circle_transfer_observation
 FOR EACH ROW EXECUTE FUNCTION circle_transfer_observation_guard();
CREATE TRIGGER circle_transfer_observation_truncate_guard BEFORE TRUNCATE ON circle_transfer_observation
 FOR EACH STATEMENT EXECUTE FUNCTION circle_transfer_observation_guard();
`

// Require the consumed append-only custody features before accepting another
// challenge submission. Later unrelated migrations do not invalidate them.
func RequireCircleTransferSchema(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	index := -1
	for i, migration := range migrations {
		if sql, ok := migration.(*SqlMigration); ok && sql.sql == circleTransferRequestSchemaSql {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("customer transfer custody is absent from this binary")
	}
	identity, err := MigrationIdentity(index)
	if err != nil {
		return err
	}
	bodies := strings.Split(circleTransferRequestSchemaSql, "$guard$")
	if len(bodies) != 5 {
		return errors.New("customer transfer guard encoding is invalid")
	}
	var ready bool
	var queryErr error
	Db(ctx, func(conn PgConn) {
		queryErr = conn.QueryRow(ctx, `SELECT
		 EXISTS(SELECT 1 FROM migration_catalog WHERE migration_index=$1 AND identity_sha256=$2)
		 AND to_regclass('public.circle_transfer_request') IS NOT NULL
		 AND to_regclass('public.circle_transfer_observation') IS NOT NULL
		 AND (SELECT count(*)=4 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid
		 WHERE t.tgenabled IN ('O','A') AND t.tgqual IS NULL AND (
		 (t.tgrelid=to_regclass('public.circle_transfer_request') AND p.prosrc=$3 AND
		 ((t.tgname='circle_transfer_request_guard' AND t.tgtype=27) OR (t.tgname='circle_transfer_request_truncate_guard' AND t.tgtype=34))) OR
		 (t.tgrelid=to_regclass('public.circle_transfer_observation') AND p.prosrc=$4 AND
		 ((t.tgname='circle_transfer_observation_guard' AND t.tgtype=27) OR (t.tgname='circle_transfer_observation_truncate_guard' AND t.tgtype=34)))))`, index, identity, bodies[1], bodies[3]).Scan(&ready)
	})
	if queryErr != nil {
		return queryErr
	}
	if !ready {
		return errors.New("customer transfer custody schema or immutable guard is missing or changed")
	}
	return nil
}
