// Each policy owns an independent signed generation sequence. The current
// projection moves between namespaces without rewriting any signed history.
package server

// This append preserves the deployed catalog and every existing signed byte.
// Retained heads identify complete archived histories; one current head per
// client selects the unsigned projection and serializes policy transitions.
const clientKeyPolicyHistorySchemaSql = `
	ALTER TABLE st_client_key_head
		DROP CONSTRAINT st_client_key_head_client_id_generation_fkey;
	ALTER TABLE st_client_key_history
		DROP CONSTRAINT st_client_key_history_pkey,
		ADD PRIMARY KEY (client_id, domain_hash, generation);
	ALTER TABLE st_client_key_head
		DROP CONSTRAINT st_client_key_head_pkey,
		ADD PRIMARY KEY (client_id, domain_hash),
		ADD COLUMN is_current boolean NOT NULL DEFAULT true,
		ADD FOREIGN KEY (client_id, domain_hash, generation)
			REFERENCES st_client_key_history(client_id, domain_hash, generation);
	CREATE UNIQUE INDEX st_client_key_head_current
		ON st_client_key_head (client_id) WHERE is_current;
	CREATE OR REPLACE FUNCTION st_client_key_head_identity_guard()
	RETURNS trigger LANGUAGE plpgsql AS $st_client_key_head_identity_guard$
	BEGIN
		IF OLD.client_id IS DISTINCT FROM NEW.client_id OR
			OLD.domain_hash IS DISTINCT FROM NEW.domain_hash OR
			OLD.network_id IS DISTINCT FROM NEW.network_id OR
			NEW.generation < OLD.generation OR
			(OLD.retired AND NOT NEW.retired) OR
			(NOT OLD.is_current AND (NEW.is_current OR NEW.generation <> OLD.generation)) THEN
			RAISE EXCEPTION 'client-key history head identity regressed';
		END IF;
		RETURN NEW;
	END
	$st_client_key_head_identity_guard$;
`
