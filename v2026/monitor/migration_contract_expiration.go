package monitor

// Migration 790 deliberately leaves old writers with NULL expiration. A
// default, nonnullable column or different timestamp semantics would change
// that contract even if migration_audit already reports the published head.
// Catalog-only lookups remain safe before the column or table exists.
const contractExpirationArtifactQuery = `EXISTS (
	SELECT 1 FROM pg_attribute a
	WHERE a.attrelid = to_regclass('public.transfer_contract')
	  AND a.attname = 'expiration_time'
	  AND a.attnum > 0 AND NOT a.attisdropped
	  AND a.atttypid = 'timestamp without time zone'::regtype
	  AND a.atttypmod = -1 AND NOT a.attnotnull
	  AND a.attgenerated = '' AND a.attidentity = ''
	  AND NOT EXISTS (
		SELECT 1 FROM pg_attrdef d
		WHERE d.adrelid = a.attrelid AND d.adnum = a.attnum
	  )
)`
