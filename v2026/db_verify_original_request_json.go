// Canonical originals retain their signed NUL domain as bytea. Only a temporary
// request-index projection omits the exact known schema member PostgreSQL cannot decode.
package server

// PostgreSQL json extraction also decodes sibling escapes, so even a json
// cast cannot read the canonical schema's NUL. Match its entire first member
// before slicing; never replace arbitrary escapes or change retained originals.
const verifyOriginalRequestIndexBodySql = `
CREATE OR REPLACE FUNCTION verify_original_request_index_body(original bytea) RETURNS jsonb
LANGUAGE plpgsql IMMUTABLE STRICT AS $original_index$
DECLARE
 prefix constant text := $schema${"schema":"urnetwork-verify-original-transition-v1\u0000",$schema$;
 encoded text;
 body jsonb;
BEGIN
 IF octet_length(original) NOT BETWEEN 1 AND 65536 THEN
  RAISE EXCEPTION 'invalid verification original index size' USING ERRCODE='22023';
 END IF;
 encoded := convert_from(original,'UTF8');
 IF left(encoded,length(prefix)) <> prefix THEN
  RAISE EXCEPTION 'invalid verification original schema prefix' USING ERRCODE='22023';
 END IF;
 body := ('{' || substring(encoded FROM length(prefix)+1))::jsonb;
 IF body ? 'schema' THEN
  RAISE EXCEPTION 'duplicate verification original schema' USING ERRCODE='22023';
 END IF;
 RETURN body;
END
$original_index$;
`

// Migration 779 repairs already-installed readers without rewriting their
// original receipts, request identities, catalog entries, or closure fences.
const verifyOriginalRequestJsonRepairSql = verifyOriginalRequestIndexBodySql +
	verifyOriginalRequestCaptureSql + verifyOriginalRequestClosedFenceSql
