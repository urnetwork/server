// The trusted instrument overlay adds two instrument functions and re-creates
// only the source authorization; all other v897 custody is carried forward.
package monitor

import "github.com/urnetwork/server"

// 900 re-creates in place the source authorization that v897 pins by exact
// body: the new body projects an authenticated instrument back to its
// selected product and checks the evaluated path base. So 900 retires v897
// and republishes its custody with the 900 body. It also requires both new
// functions with their exact bodies and metadata.
var competitionMigration900ArtifactQuery = func() string {
	lockDigest, productIntent, sourceAuthorized := server.CompetitionTrustedInstrumentBodiesSql()
	return "(" +
		snMainnetMigrationFunction("competition_v2_instrument_lock_digest(jsonb)", "text", "sql", "i", false, lockDigest, "commits") + " AND " +
		snMainnetMigrationFunction("competition_v2_instrument_product_intent(jsonb,text,text,text)", "jsonb", "plpgsql", "v", false, productIntent, "intent", "env", "competition", "season") + " AND " +
		competitionSnowTwoMemberCustodyArtifactQuery(sourceAuthorized) + ")"
}()
