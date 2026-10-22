// These are artifact contracts for the single migrations signal, not separate
// probes. Nullable catalog lookups must also work before versions 749–751.
package monitor

import "strings"

const clientRegistrationArtifactQuery = `(
	EXISTS (SELECT 1 FROM pg_class WHERE oid=to_regclass('public.network_client_registration')
		AND relkind='r' AND relpersistence='p')
	AND (SELECT count(*) = 9 FROM (VALUES
		('network_id','uuid',NULL::integer), ('registration_id','character varying',64),
		('request_sha256','character varying',64), ('scope_sha256','character varying',64),
		('authority_sha256','character varying',64), ('user_id','uuid',NULL),
		('client_id','uuid',NULL), ('device_id','uuid',NULL),
		('create_time','timestamp without time zone',NULL)
	) AS expected(column_name,data_type,maximum_length)
	WHERE EXISTS (SELECT 1 FROM information_schema.columns AS actual
		WHERE actual.table_schema='public' AND actual.table_name='network_client_registration'
			AND actual.column_name=expected.column_name AND actual.data_type=expected.data_type
			AND actual.character_maximum_length IS NOT DISTINCT FROM expected.maximum_length
			AND actual.is_nullable='NO' AND actual.column_default IS NULL
			AND actual.is_identity='NO' AND actual.is_generated='NEVER'))
	AND (SELECT count(*) = 3 FROM (VALUES
		('p','PRIMARY KEY (network_id, registration_id)'),
		('u','UNIQUE (network_id, scope_sha256)'), ('u','UNIQUE (client_id)')
	) AS expected(kind,definition)
	WHERE EXISTS (SELECT 1 FROM pg_constraint AS actual
		JOIN pg_index AS backing ON backing.indexrelid=actual.conindid
		WHERE actual.conrelid=to_regclass('public.network_client_registration')
			AND actual.contype::text=expected.kind AND actual.convalidated
			AND NOT actual.condeferrable AND NOT actual.condeferred
			AND pg_get_constraintdef(actual.oid)=expected.definition
			AND backing.indisvalid AND backing.indisready AND backing.indislive
			AND backing.indpred IS NULL))
	AND (SELECT count(*) = 4 FROM (VALUES
		('registration_id'), ('request_sha256'), ('scope_sha256'), ('authority_sha256')
	) AS expected(column_name)
	WHERE EXISTS (SELECT 1 FROM pg_constraint AS actual
		WHERE actual.conrelid=to_regclass('public.network_client_registration')
			AND actual.contype='c' AND actual.convalidated
			AND pg_get_constraintdef(actual.oid)=
				'CHECK (((' || expected.column_name || ')::text ~ ''^[0-9a-f]{64}$''::text))'))
	-- The binding survives user/client/device deletion; only network deletion
	-- cascades. An added client FK would silently change that retention rule.
	AND (SELECT count(*) = 1 FROM pg_constraint
		WHERE conrelid=to_regclass('public.network_client_registration') AND contype='f')
	AND EXISTS (SELECT 1 FROM pg_constraint AS actual
		WHERE actual.conrelid=to_regclass('public.network_client_registration') AND actual.contype='f'
			AND actual.confrelid=to_regclass('public.network')
			AND actual.convalidated AND NOT actual.condeferrable AND NOT actual.condeferred
			AND actual.confdeltype='c' AND actual.confupdtype='a' AND actual.confmatchtype='s'
			AND actual.conkey=ARRAY[(SELECT attnum FROM pg_attribute
				WHERE attrelid=to_regclass('public.network_client_registration') AND attname='network_id' AND NOT attisdropped)]
			AND actual.confkey=ARRAY[(SELECT attnum FROM pg_attribute
				WHERE attrelid=to_regclass('public.network') AND attname='network_id' AND NOT attisdropped)])
)`

const subscriberQualityVerifiedArtifactQuery = `EXISTS (
	SELECT 1 FROM information_schema.columns
	WHERE table_schema='public' AND table_name='network_client_location'
		AND column_name='arin_quality_verified' AND data_type='boolean'
		AND is_nullable='NO' AND column_default='false'
		AND is_identity='NO' AND is_generated='NEVER'
)`

// This body intentionally pins the published v751 function. It does not read
// subscriber rows or infer whether the separate activation policy is enabled.
const subscriberQualityGuardFunctionBody = `
	BEGIN
		IF TG_OP = 'INSERT' THEN
			IF NEW.arin_quality_write_token IS NULL THEN
				NEW.arin_quality_verified := false;
			END IF;
		ELSIF NEW.arin_quality_write_token IS NULL OR
			NEW.arin_quality_write_token IS NOT DISTINCT FROM OLD.arin_quality_write_token THEN
			NEW.arin_quality_verified := false;
			NEW.arin_quality_write_token := OLD.arin_quality_write_token;
		END IF;
		RETURN NEW;
	END
	`

var subscriberQualityWriteGuardArtifactQuery = `(
	EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema='public' AND table_name='network_client_location'
			AND column_name='arin_quality_write_token' AND data_type='uuid'
			AND is_nullable='YES' AND column_default IS NULL
			AND is_identity='NO' AND is_generated='NEVER')
	AND EXISTS (SELECT 1 FROM pg_proc AS actual
		WHERE actual.oid=to_regprocedure('public.network_client_location_subscriber_quality_guard()')
			AND actual.prorettype='trigger'::regtype AND actual.prokind='f'
			AND NOT actual.prosecdef AND actual.proconfig IS NULL
			AND actual.prosrc='` + strings.ReplaceAll(subscriberQualityGuardFunctionBody, "'", "''") + `'
			AND actual.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql'))
	AND EXISTS (SELECT 1 FROM pg_trigger AS actual
		WHERE actual.tgrelid=to_regclass('public.network_client_location')
			AND actual.tgname='network_client_location_subscriber_quality_guard'
			AND actual.tgfoid=to_regprocedure('public.network_client_location_subscriber_quality_guard()')
			AND actual.tgtype=23 AND actual.tgenabled='O'
			AND actual.tgnargs=0 AND actual.tgqual IS NULL AND actual.tgattr=''::int2vector
			AND actual.tgoldtable IS NULL AND actual.tgnewtable IS NULL AND NOT actual.tgisinternal)
)`
