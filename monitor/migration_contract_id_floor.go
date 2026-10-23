// Version 856 adds the contract id floor (CONTRACT-PARTITION.md).
package monitor

import (
	"fmt"

	"github.com/urnetwork/server"
)

// The partition bounds and the legacy bound check depend on an IMMUTABLE,
// STRICT function with this exact body; parallel safety is part of the
// published definition.
var contractIdFloorArtifactQuery = fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_proc p
 WHERE p.oid=to_regprocedure('public.contract_id_floor(timestamp with time zone)') AND p.prorettype='uuid'::regtype
 AND p.prolang=(SELECT oid FROM pg_language WHERE lanname='sql')
 AND p.prokind='f' AND p.provolatile='i' AND p.proparallel='s' AND p.proisstrict
 AND NOT p.prosecdef AND NOT p.proleakproof AND NOT p.proretset AND p.pronargdefaults=0
 AND p.proargnames IS NOT DISTINCT FROM ARRAY['t']::text[]
 AND p.proconfig IS NULL AND p.prosrc=%s)`, snMainnetMigrationLiteral(server.ContractIdFloorBodySql()))
