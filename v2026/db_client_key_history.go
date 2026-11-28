// Client-key history survives Redis eviction, explicit key clearing and client
// reaping. Signed bytes are append-only; the mutable head is only their exact
// generation index, never a substitute for checking those bytes.
package server

// Appended to the ordinary migration catalog; no prior migration identity is
// rewritten. There is deliberately no cascading foreign key to network_client.
const clientKeyHistorySchemaSQL = `
	CREATE TABLE st_client_key_history (
		client_id uuid NOT NULL,
		generation bigint NOT NULL CHECK (generation > 0),
		domain_hash bytea NOT NULL CHECK (octet_length(domain_hash) = 32),
		registration_hash bytea NOT NULL CHECK (octet_length(registration_hash) = 32),
		registration bytea NOT NULL CHECK (octet_length(registration) BETWEEN 1 AND 8192),
		evidence_hash text NOT NULL CHECK (evidence_hash ~ '^sha256:[0-9a-f]{64}$'),
		evidence bytea NOT NULL CHECK (octet_length(evidence) BETWEEN 1 AND 16384),
		PRIMARY KEY (client_id, generation),
		UNIQUE (registration_hash)
	);
	CREATE TABLE st_client_key_head (
		client_id uuid PRIMARY KEY,
		domain_hash bytea NOT NULL CHECK (octet_length(domain_hash) = 32),
		network_id uuid NOT NULL,
		generation bigint NOT NULL CHECK (generation > 0),
		retired boolean NOT NULL DEFAULT false,
		FOREIGN KEY (client_id, generation) REFERENCES st_client_key_history(client_id, generation)
	);
	CREATE FUNCTION st_client_key_history_immutable_guard()
	RETURNS trigger LANGUAGE plpgsql AS $st_client_key_history_immutable_guard$
	BEGIN
		RAISE EXCEPTION 'signed client-key history is immutable';
	END
	$st_client_key_history_immutable_guard$;
	CREATE TRIGGER st_client_key_history_immutable
	BEFORE UPDATE OR DELETE ON st_client_key_history
	FOR EACH ROW EXECUTE FUNCTION st_client_key_history_immutable_guard();
	CREATE FUNCTION st_client_key_head_identity_guard()
	RETURNS trigger LANGUAGE plpgsql AS $st_client_key_head_identity_guard$
	BEGIN
		IF OLD.client_id IS DISTINCT FROM NEW.client_id OR
			OLD.domain_hash IS DISTINCT FROM NEW.domain_hash OR
			OLD.network_id IS DISTINCT FROM NEW.network_id OR
			NEW.generation < OLD.generation OR
			(OLD.retired AND NOT NEW.retired) THEN
			RAISE EXCEPTION 'client-key history head identity regressed';
		END IF;
		RETURN NEW;
	END
	$st_client_key_head_identity_guard$;
	CREATE TRIGGER st_client_key_head_identity
	BEFORE UPDATE ON st_client_key_head
	FOR EACH ROW EXECUTE FUNCTION st_client_key_head_identity_guard();
	CREATE FUNCTION st_client_key_retire_deleted_client()
	RETURNS trigger LANGUAGE plpgsql AS $st_client_key_retire_deleted_client$
	BEGIN
		UPDATE st_client_key_head SET retired = true WHERE client_id = OLD.client_id;
		RETURN OLD;
	END
	$st_client_key_retire_deleted_client$;
	CREATE TRIGGER st_client_key_retire_on_client_delete
	AFTER DELETE ON network_client
	FOR EACH ROW EXECUTE FUNCTION st_client_key_retire_deleted_client();
`
