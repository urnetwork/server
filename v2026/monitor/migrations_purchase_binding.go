// Published store bindings retain their exact transaction and purchase keys.
package monitor

var appleOfferCodeBindingArtifactQuery = `(` +
	migrationFp2Table("apple_offer_code_binding") + ` AND ` +
	financialColumnsArtifact("apple_offer_code_binding", `('original_transaction_id','character varying','NO',NULL),('network_id','uuid','NO',NULL),('transaction_id','character varying','NO',NULL),('offer_identifier','character varying','NO',NULL),('bound_at','timestamp without time zone','NO',NULL)`) + ` AND ` +
	financialConstraintsArtifact("apple_offer_code_binding", `('PRIMARY KEY (original_transaction_id)')`) + ` AND
 (SELECT count(*)=3 FROM information_schema.columns
 WHERE table_schema='public' AND table_name='apple_offer_code_binding'
 AND column_name IN ('original_transaction_id','transaction_id','offer_identifier')
 AND character_maximum_length=128))`

var playPurchaseBindingArtifactQuery = `(` +
	migrationFp2Table("play_purchase_binding") + ` AND ` +
	financialColumnsArtifact("play_purchase_binding", `('purchase_token','text','NO',NULL),('root_purchase_token','text','NO',NULL),('network_id','uuid','NO',NULL),('offer','character varying','NO',NULL),('bound_at','timestamp without time zone','NO',NULL)`) + ` AND ` +
	financialConstraintsArtifact("play_purchase_binding", `('PRIMARY KEY (purchase_token)')`) + ` AND
 EXISTS(SELECT 1 FROM information_schema.columns
 WHERE table_schema='public' AND table_name='play_purchase_binding'
 AND column_name='offer' AND character_maximum_length=256))`
