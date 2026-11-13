// Positive subscriber facts belong to one location write, not a connection's
// lifetime. Keep mixed-generation updates compatible but fail closed.
package server

// Append after schema 750: already-applied migration identities must not change.
// Reset unbound positives under the same table lock as trigger installation.
// The nullable token has no default; only an aware writer can supply a new one.
const subscriberQualityWriteGuardSchemaSql = `
	ALTER TABLE network_client_location
		ADD COLUMN arin_quality_write_token uuid;
	UPDATE network_client_location SET arin_quality_verified = false
		WHERE arin_quality_verified;
	CREATE FUNCTION network_client_location_subscriber_quality_guard() RETURNS trigger LANGUAGE plpgsql AS $subscriber_quality$
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
	$subscriber_quality$;
	CREATE TRIGGER network_client_location_subscriber_quality_guard
		BEFORE INSERT OR UPDATE ON network_client_location
		FOR EACH ROW EXECUTE FUNCTION network_client_location_subscriber_quality_guard();
`
