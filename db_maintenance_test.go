package server

import (
	"slices"
	"testing"

	"github.com/urnetwork/connect"
)

func TestDbMaintenanceSkipsWholeReindexForLargeHighChurnTables(t *testing.T) {
	for _, tableName := range []string{
		"contract_participant",
		"contract_close",
		"network_client",
		"transfer_contract",
		"transfer_escrow",
		"transfer_escrow_sweep",
	} {
		if dbMaintenanceShouldReindexTable(tableName) {
			t.Fatalf("%s would still be rebuilt as a whole", tableName)
		}
	}
	opts := DefaultDbMaintenanceOptions()
	if !opts.Cleanup || !opts.Analyze || !opts.Reindex {
		t.Fatal("table exclusion changed the default maintenance actions")
	}
	steps := dbMaintenanceObjectSteps(opts.Cleanup, opts.Reindex && dbMaintenanceShouldReindexTable("network_client"))
	if !slices.Equal(steps, []dbMaintenanceObjectStep{dbMaintenanceCleanupBefore}) {
		t.Fatalf("excluded network_client steps = %v, want cleanup without a whole-table rebuild", steps)
	}
	if !dbMaintenanceShouldReindexTable("account_feedback") {
		t.Fatal("ordinary table was excluded from the reindex rotation")
	}
}

// The compact estimate follows the btree leaf layout: header, null bitmap,
// aligned column data less expected nulls, line pointer, fillfactor, and the
// metapage. Each expected size is worked out by hand from that layout.
func TestDbMaintenanceEstimateBtreeBytesFollowsTheLeafLayout(t *testing.T) {
	uuidColumn := dbMaintenanceIndexColumn{length: 16, align: 'c', analyzed: true, averageWidth: 16}
	timestampColumn := dbMaintenanceIndexColumn{length: 8, align: 'd', analyzed: true, averageWidth: 8}
	bigintColumn := dbMaintenanceIndexColumn{length: 8, align: 'd', analyzed: true, averageWidth: 8}
	integerColumn := dbMaintenanceIndexColumn{length: 4, align: 'i', analyzed: true, averageWidth: 4}
	halfNullUuidColumn := dbMaintenanceIndexColumn{length: 16, align: 'c', analyzed: true, averageWidth: 16, nullFraction: 0.5}
	for _, c := range []struct {
		name       string
		columns    []dbMaintenanceIndexColumn
		entryCount float64
		fillfactor int
		wantBytes  int64
		wantOk     bool
	}{
		// 8 + 16 + 16 + 8 = 48 + 4: 141 entries per 7336 usable bytes, 7093 leaves
		{
			name:       "two uuids and an included bigint",
			columns:    []dbMaintenanceIndexColumn{uuidColumn, uuidColumn, bigintColumn},
			entryCount: 1000000,
			fillfactor: 90,
			wantBytes:  (7093 + 1) * 8192,
			wantOk:     true,
		},
		// 8 + 8 + 4 = 20: 366 per page
		{
			name:       "one timestamp",
			columns:    []dbMaintenanceIndexColumn{timestampColumn},
			entryCount: 146982096,
			fillfactor: 90,
			wantBytes:  (401591 + 1) * 8192,
			wantOk:     true,
		},
		// at fillfactor 100: 8152 / 20 = 407 per page
		{
			name:       "one timestamp packed",
			columns:    []dbMaintenanceIndexColumn{timestampColumn},
			entryCount: 1000,
			fillfactor: 100,
			wantBytes:  (3 + 1) * 8192,
			wantOk:     true,
		},
		// 84 data bytes less 8 expected null bytes, 16 with the null bitmap:
		// 92 aligns to 96, + 4 = 100: 73 per page, 27398 leaves
		{
			name: "nullable pair layout",
			columns: []dbMaintenanceIndexColumn{
				uuidColumn,
				uuidColumn,
				timestampColumn,
				uuidColumn,
				halfNullUuidColumn,
				bigintColumn,
				integerColumn,
			},
			entryCount: 2000000,
			fillfactor: 90,
			wantBytes:  (27398 + 1) * 8192,
			wantOk:     true,
		},
		// an analyzed variable width uses its average: 8 + 20 = 28 aligns to 32, + 4 = 36
		{
			name:       "analyzed variable width",
			columns:    []dbMaintenanceIndexColumn{{length: -1, align: 'i', analyzed: true, averageWidth: 20}},
			entryCount: 1000000,
			fillfactor: 90,
			wantBytes:  (4927 + 1) * 8192,
			wantOk:     true,
		},
		{
			name:       "empty index",
			columns:    []dbMaintenanceIndexColumn{uuidColumn},
			entryCount: 0,
			fillfactor: 90,
			wantBytes:  8192,
			wantOk:     true,
		},
		{
			name:       "variable width never analyzed",
			columns:    []dbMaintenanceIndexColumn{uuidColumn, {length: -1, align: 'i'}},
			entryCount: 1000,
			fillfactor: 90,
			wantOk:     false,
		},
		{
			name:       "never vacuumed or analyzed",
			columns:    []dbMaintenanceIndexColumn{uuidColumn},
			entryCount: -1,
			fillfactor: 90,
			wantOk:     false,
		},
	} {
		gotBytes, gotOk := dbMaintenanceEstimateBtreeBytes(c.columns, c.entryCount, c.fillfactor, 8192)
		if gotOk != c.wantOk || gotBytes != c.wantBytes {
			t.Errorf("%s: estimate = (%d, %t), want (%d, %t)", c.name, gotBytes, gotOk, c.wantBytes, c.wantOk)
		}
	}
}

// A priority index is rebuilt when at least half of it is reclaimable and the
// reclaim pays for reading its heap twice; the rebuilds go smallest first,
// skip the indexes done earlier in the epoch, and stop at the per-run bound.
func TestDbMaintenanceSelectPriorityIndexesRebuildsBloatedSmallestFirst(t *testing.T) {
	const gib = int64(1024 * 1024 * 1024)
	const mib = int64(1024 * 1024)
	priorityIndexes := []dbMaintenancePriorityIndex{
		// below the bloat factor
		{indexName: "synthetic_compact", indexBytes: 39 * gib, compactBytes: 20 * gib, heapBytes: 400 * gib},
		// reclaims less than a hundredth of the heap it reads twice
		{indexName: "synthetic_heap_bound", indexBytes: 10 * gib, compactBytes: 1 * gib, heapBytes: 1000 * gib},
		// reclaims less than the absolute floor
		{indexName: "synthetic_tiny", indexBytes: 768 * 1024, compactBytes: 8192},
		{indexName: "synthetic_unknown", indexBytes: 50 * gib, compactBytes: 0, heapBytes: 400 * gib},
		{indexName: "synthetic_large", indexBytes: 125 * gib, compactBytes: 8 * gib, heapBytes: 437 * gib},
		{indexName: "synthetic_medium", indexBytes: 38 * gib, compactBytes: 7 * gib, heapBytes: 481 * gib},
		{indexName: "synthetic_small", indexBytes: 15 * gib, compactBytes: 3 * gib, heapBytes: 481 * gib},
		{indexName: "synthetic_done", indexBytes: 19 * gib, compactBytes: 4 * gib, heapBytes: 481 * gib},
		// exactly at both bounds
		{indexName: "synthetic_boundary", indexBytes: 4 * mib, compactBytes: 2 * mib, heapBytes: 200 * mib},
	}
	names := func(selectedIndexes []dbMaintenancePriorityIndex) []string {
		selectedNames := []string{}
		for _, selectedIndex := range selectedIndexes {
			selectedNames = append(selectedNames, selectedIndex.indexName)
		}
		return selectedNames
	}
	done := []string{"synthetic_done"}
	for _, c := range []struct {
		maxRebuilds int
		want        []string
	}{
		{maxRebuilds: 10, want: []string{"synthetic_boundary", "synthetic_small", "synthetic_medium", "synthetic_large"}},
		{maxRebuilds: 2, want: []string{"synthetic_boundary", "synthetic_small"}},
		{maxRebuilds: 0, want: []string{}},
	} {
		got := names(dbMaintenanceSelectPriorityIndexes(priorityIndexes, done, c.maxRebuilds))
		if !slices.Equal(got, c.want) {
			t.Errorf("at most %d: selected %v, want %v", c.maxRebuilds, got, c.want)
		}
	}

	// equal sizes go in name order
	tied := []dbMaintenancePriorityIndex{
		{indexName: "synthetic_b", indexBytes: 40 * gib, compactBytes: 4 * gib, heapBytes: 100 * gib},
		{indexName: "synthetic_a", indexBytes: 40 * gib, compactBytes: 4 * gib, heapBytes: 100 * gib},
	}
	if got := names(dbMaintenanceSelectPriorityIndexes(tied, nil, 10)); !slices.Equal(got, []string{"synthetic_a", "synthetic_b"}) {
		t.Errorf("tied sizes selected %v, want name order", got)
	}
}

// The monitor tells a priority rebuild from a whole-table rebuild by the index
// in progress: a priority index or the transient copy of one.
func TestIsDbMaintenancePriorityIndexMatchesPriorityIndexesAndTheirCopies(t *testing.T) {
	longest := ""
	for _, name := range priorityReindexIndexes {
		if len(longest) < len(name) {
			longest = name
		}
	}
	for _, c := range []struct {
		indexName string
		want      bool
	}{
		{indexName: "transfer_escrow_unsettled_balance_contract", want: true},
		{indexName: "public.transfer_escrow_unsettled_balance_contract", want: true},
		{indexName: "transfer_escrow_unsettled_balance_contract_ccnew", want: true},
		{indexName: "transfer_escrow_unsettled_balance_contract_ccold2", want: true},
		{indexName: reindexArtifactName(longest, "ccnew"), want: true},
		{indexName: reindexArtifactName(longest, "ccnew12"), want: true},
		{indexName: "transfer_escrow_pkey_ccnew36", want: false},
		{indexName: "transfer_escrow_pkey", want: false},
		{indexName: "transfer_escrow", want: false},
		{indexName: "transfer_escrow_unsettled_balance_contract_ccnew0", want: false},
	} {
		if got := IsDbMaintenancePriorityIndex(c.indexName); got != c.want {
			t.Errorf("IsDbMaintenancePriorityIndex(%q) = %t, want %t", c.indexName, got, c.want)
		}
	}
}

func TestDbMaintenanceObjectStepsGuardEveryDefaultReindex(t *testing.T) {
	want := []dbMaintenanceObjectStep{
		dbMaintenanceCleanupBefore,
		dbMaintenanceReindex,
		dbMaintenanceCleanupAfter,
	}
	if got := dbMaintenanceObjectSteps(true, true); !slices.Equal(got, want) {
		t.Fatalf("default maintenance steps = %v, want %v", got, want)
	}

	wantCleanupOnly := []dbMaintenanceObjectStep{dbMaintenanceCleanupBefore}
	if got := dbMaintenanceObjectSteps(true, false); !slices.Equal(got, wantCleanupOnly) {
		t.Fatalf("cleanup-only maintenance steps = %v, want %v", got, wantCleanupOnly)
	}

	wantReindexOnly := []dbMaintenanceObjectStep{dbMaintenanceReindex}
	if got := dbMaintenanceObjectSteps(false, true); !slices.Equal(got, wantReindexOnly) {
		t.Fatalf("reindex-only maintenance steps = %v, want %v", got, wantReindexOnly)
	}
}

func TestDbMaintenanceObjectStepsCleanFailedReindexBeforeNextAttempt(t *testing.T) {
	steps := dbMaintenanceObjectSteps(true, true)

	run := []dbMaintenanceObjectStep{}
	runDbMaintenanceObjectSteps(steps, func(step dbMaintenanceObjectStep) bool {
		run = append(run, step)
		return step != dbMaintenanceReindex
	})
	want := []dbMaintenanceObjectStep{
		dbMaintenanceCleanupBefore,
		dbMaintenanceReindex,
		dbMaintenanceCleanupAfter,
	}
	if !slices.Equal(run, want) {
		t.Fatalf("steps after reindex failure = %v, want cleanup after failure %v", run, want)
	}

	run = nil
	runDbMaintenanceObjectSteps(steps, func(step dbMaintenanceObjectStep) bool {
		run = append(run, step)
		return false
	})
	want = []dbMaintenanceObjectStep{dbMaintenanceCleanupBefore}
	if !slices.Equal(run, want) {
		t.Fatalf("steps after prerequisite cleanup failure = %v, want no reindex %v", run, want)
	}
}

func TestIncompleteIndexName(t *testing.T) {

	incompleteIndexNames := []string{
		"account_feedback_pkey_ccnew",
		"pg_toast_16818_index_ccnew",
		"audit_network_event_pkey_ccnew",
		"audit_network_event_stats_network_id_ccnew",
		"pg_toast_16879_index_ccnew",
		"network_client_connection_pkey_ccnew",
		"network_client_connection_handler_id_ccnew",
		"network_client_connection_connected_client_id_ccnew",
		"network_client_connection_connected_disconnect_time_ccnew",
		"network_client_connection_client_address_hash_connected_ccnew",
		"network_client_connection_connected_client_id_ccnew1",
		"network_client_connection_connected_disconnect_time_ccnew1",
		"network_client_connection_client_address_hash_connected_ccnew1",
		"network_client_connection_pkey_ccnew1",
		"network_client_connection_handler_id_ccnew1",
		"network_client_connection_pkey_ccnew2",
		"network_client_connection_handler_id_ccnew2",
		"network_client_connection_connected_client_id_ccnew2",
		"network_client_connection_connected_disconnect_time_ccnew2",
		"network_client_connection_client_address_hash_connected_ccnew2",
		"network_client_connection_pkey_ccnew3",
		"network_client_connection_handler_id_ccnew3",
		"network_client_connection_connected_client_id_ccnew3",
		"network_client_connection_connected_disconnect_time_ccnew3",
		"network_client_connection_client_address_hash_connected_ccnew3",
		"network_client_resident_pkey_ccnew",
		"network_client_resident_client_id_key_ccnew",
		"location_pkey_ccnew",
		"location_location_full_name_key_ccnew",
		"location_type_country_code_name_ccnew",
		"location_group_pkey_ccnew",
		"location_group_location_group_name_key_ccnew",
		"location_group_pkey_ccnew1",
		"location_group_location_group_name_key_ccnew1",
		"auth_code_session_pkey_ccnew",
		"device_adopt_pkey_ccnew",
		"device_adopt_owner_network_id_ccnew",
		"device_adopt_pkey_ccnew1",
		"device_adopt_owner_network_id_ccnew1",
		"device_adopt_pkey_ccnew2",
		"device_adopt_owner_network_id_ccnew2",
		"pg_toast_11853511_index_ccnew",
		"pg_toast_11853511_index_ccnew1",
		"pg_toast_11853511_index_ccnew2",
		"device_adopt_auth_session_pkey_ccnew",
		"network_client_location_reliability_pkey_ccnew",
		"network_client_location_reliability_update_block_number_c_ccnew",
		"network_client_location_reliability_valid_connected_clien_ccnew",
		"network_client_location_reliability_pkey_ccnew2",
		"network_client_location_reliability_update_block_number__ccnew2",
		"network_client_location_reliability_valid_connected_clie_ccnew2",
		"network_client_location_reliability_pkey_ccnew1",
		"network_client_location_reliability_update_block_number__ccnew1",
		"network_client_location_reliability_valid_connected_clie_ccnew1",
		"pg_toast_16931_index_ccnew",
		"pg_toast_16931_index_ccnew1",
		"pg_toast_16931_index_ccnew2",
		"pg_toast_16931_index_ccnew3",
	}

	completeIndexNames := []string{
		"pg_default_acl_role_nsp_obj_index",
		"pg_default_acl_oid_index",
		"pg_tablespace_spcname_index",
		"pg_tablespace_oid_index",
		"pg_shdepend_depender_index",
		"pg_shdepend_reference_index",
		"pg_type_typname_nsp_index",
		"pg_type_oid_index",
		"pg_attribute_relid_attnam_index",
		"pg_attribute_relid_attnum_index",
		"pg_proc_proname_args_nsp_index",
		"pg_proc_oid_index",
		"pg_class_relname_nsp_index",
		"pg_class_tblspc_relfilenode_index",
		"pg_class_oid_index",
		"pg_authid_rolname_index",
		"pg_authid_oid_index",
		"pg_auth_members_role_member_index",
		"pg_auth_members_member_role_index",
		"pg_auth_members_grantor_index",
		"pg_auth_members_oid_index",
		"pg_database_datname_index",
		"pg_database_oid_index",
		"pg_foreign_server_name_index",
		"pg_foreign_server_oid_index",
		"pg_user_mapping_user_server_index",
		"pg_user_mapping_oid_index",
		"pg_sequence_seqrelid_index",
		"pg_foreign_data_wrapper_name_index",
		"pg_foreign_data_wrapper_oid_index",
		"pg_toast_2620_index",
		"pg_shdescription_o_c_index",
		"pg_aggregate_fnoid_index",
		"pg_am_name_index",
		"pg_am_oid_index",
		"pg_amop_fam_strat_index",
		"pg_amop_opr_fam_index",
		"pg_amop_oid_index",
		"pg_amproc_fam_proc_index",
		"pg_amproc_oid_index",
		"pg_attrdef_adrelid_adnum_index",
		"pg_attrdef_oid_index",
		"pg_cast_source_target_index",
		"pg_cast_oid_index",
		"pg_constraint_conname_nsp_index",
		"pg_constraint_conrelid_contypid_conname_index",
		"pg_constraint_contypid_index",
		"pg_constraint_conparentid_index",
		"pg_constraint_oid_index",
		"pg_conversion_default_index",
		"pg_conversion_name_nsp_index",
		"pg_conversion_oid_index",
		"pg_depend_depender_index",
		"pg_depend_reference_index",
		"pg_description_o_c_o_index",
		"pg_index_indrelid_index",
		"pg_index_indexrelid_index",
		"pg_inherits_parent_index",
		"pg_inherits_relid_seqno_index",
		"pg_language_name_index",
		"pg_language_oid_index",
		"pg_largeobject_loid_pn_index",
		"pg_namespace_nspname_index",
		"pg_namespace_oid_index",
		"pg_opclass_am_name_nsp_index",
		"pg_opclass_oid_index",
		"pg_operator_oprname_l_r_n_index",
		"pg_operator_oid_index",
		"pg_rewrite_rel_rulename_index",
		"pg_rewrite_oid_index",
		"pg_statistic_relid_att_inh_index",
		"pg_trigger_tgconstraint_index",
		"pg_trigger_tgrelid_tgname_index",
		"pg_trigger_oid_index",
		"pg_opfamily_am_name_nsp_index",
		"pg_opfamily_oid_index",
		"pg_toast_2604_index",
		"pg_toast_2606_index",
		"pg_toast_2609_index",
		"pg_toast_1255_index",
		"pg_toast_2618_index",
		"pg_toast_2619_index",
		"pg_toast_2396_index",
		"pg_db_role_setting_databaseid_rol_index",
		"pg_toast_2964_index",
		"pg_largeobject_metadata_oid_index",
		"pg_extension_name_index",
		"pg_extension_oid_index",
		"pg_foreign_table_relid_index",
		"pg_policy_polrelid_polname_index",
		"pg_policy_oid_index",
		"pg_partitioned_table_partrelid_index",
		"pg_statistic_ext_name_index",
		"pg_statistic_ext_relid_index",
		"pg_statistic_ext_oid_index",
		"pg_init_privs_o_c_o_index",
		"pg_statistic_ext_data_stxoid_inh_index",
		"pg_toast_3429_index",
		"pg_toast_3381_index",
		"pg_collation_name_enc_nsp_index",
		"pg_collation_oid_index",
		"pg_event_trigger_evtname_index",
		"pg_event_trigger_oid_index",
		"pg_enum_typid_label_index",
		"pg_enum_oid_index",
		"pg_enum_typid_sortorder_index",
		"pg_range_rngmultitypid_index",
		"pg_range_rngtypid_index",
		"pg_transform_type_lang_index",
		"pg_transform_oid_index",
		"pg_shseclabel_object_index",
		"pg_seclabel_object_index",
		"pg_toast_3596_index",
		"pg_ts_dict_dictname_index",
		"pg_ts_dict_oid_index",
		"pg_ts_parser_prsname_index",
		"pg_ts_parser_oid_index",
		"pg_ts_config_cfgname_index",
		"pg_ts_config_oid_index",
		"pg_ts_config_map_index",
		"pg_ts_template_tmplname_index",
		"pg_ts_template_oid_index",
		"pg_toast_3592_index",
		"pg_toast_826_index",
		"pg_toast_3466_index",
		"pg_toast_3079_index",
		"pg_toast_2328_index",
		"pg_toast_1417_index",
		"pg_toast_3118_index",
		"pg_toast_3394_index",
		"pg_toast_2612_index",
		"pg_toast_2600_index",
		"pg_toast_2615_index",
		"pg_toast_3350_index",
		"pg_toast_3256_index",
		"pg_toast_3600_index",
		"pg_toast_1247_index",
		"pg_toast_1418_index",
		"pg_toast_1262_index",
		"pg_toast_6100_index",
		"pg_toast_1213_index",
		"pg_replication_origin_roname_index",
		"pg_replication_origin_roiident_index",
		"pg_subscription_subname_index",
		"pg_subscription_oid_index",
		"pg_subscription_rel_srrelid_srsubid_index",
		"pg_publication_pubname_index",
		"pg_publication_oid_index",
		"pg_publication_rel_prrelid_prpubid_index",
		"pg_publication_rel_prpubid_index",
		"pg_publication_rel_oid_index",
		"pg_toast_3456_index",
		"pg_toast_6106_index",
		"pg_publication_namespace_pnnspid_pnpubid_index",
		"pg_publication_namespace_oid_index",
		"pg_parameter_acl_parname_index",
		"pg_parameter_acl_oid_index",
		"pg_toast_6243_index",
		"pg_toast_2610_index",
		"pg_toast_13393_index",
		"pg_toast_13398_index",
		"pg_toast_13403_index",
		"pg_toast_13408_index",
		"account_balance_pkey",
		"account_feedback_pkey",
		"pg_toast_16818_index",
		"account_payment_completed_complete_time",
		"account_payment_completed_complete_time_contract_id",
		"account_payment_network_id_canceled",
		"account_payment_payment_plan",
		"account_payment_pkey",
		"pg_toast_16840_index",
		"account_preferences_pkey",
		"account_wallet_active_network_id",
		"account_wallet_pkey",
		"unique_network_wallet_address",
		"pg_toast_16852_index",
		"audit_contract_event_event_id_key",
		"audit_contract_event_stats",
		"audit_contract_event_stats_extender_id",
		"pg_toast_16859_index",
		"audit_device_event_event_id_key",
		"audit_device_event_pkey",
		"audit_device_event_stats_device_id",
		"pg_toast_16867_index",
		"audit_extender_event_pkey",
		"audit_extender_event_stats_extender_id",
		"pg_toast_16873_index",
		"audit_network_event_pkey",
		"audit_network_event_stats_network_id",
		"pg_toast_16879_index",
		"audit_provider_event_pkey",
		"audit_provider_event_stats_device_id",
		"pg_toast_16885_index",
		"contract_close_pkey",
		"location_group_member_location_id",
		"location_group_member_pkey",
		"network_admin_user_id",
		"network_network_name_key",
		"network_pkey",
		"network_product_updates_sync_admin_user_id",
		"network_client_pkey",
		"network_client_device_id",
		"network_client_active_network_id_create_time",
		"network_client_connection_connected_disconnect_time",
		"network_client_connection_client_address_hash_connected",
		"network_client_connection_pkey",
		"network_client_connection_handler_id",
		"network_client_connection_connected_client_id",
		"network_client_location_pkey",
		"network_client_location_client_id",
		"network_client_resident_pkey",
		"network_client_resident_client_id_key",
		"network_client_resident_port_pkey",
		"network_user_pkey",
		"network_user_user_auth_key",
		"network_user_wallet_address_unique",
		"payout_wallet_pkey",
		"provide_key_pkey",
		"provide_key_provide_mode_client_id",
		"pg_toast_16953_index",
		"search_projection_pkey",
		"search_projection_value",
		"search_value_pkey",
		"search_value_realm_vlen",
		"pg_toast_16962_index",
		"transfer_balance_network_id",
		"transfer_balance_pkey",
		"transfer_balance_code_balance_code_secret_key",
		"transfer_balance_code_pkey",
		"transfer_balance_code_purchase_event_id_key",
		"transfer_balance_code_redeemed_time",
		"pg_toast_16974_index",
		"transfer_contract_pkey",
		"transfer_contract_open_source_id",
		"transfer_contract_open_create_time",
		"transfer_escrow_pkey",
		"transfer_escrow_sweep_network_id_payment_id_payout",
		"transfer_escrow_sweep_payment_id",
		"transfer_escrow_sweep_pkey",
		"user_auth_attempt_attempt_time_user_auth_attempt_id",
		"user_auth_attempt_client_address_hash_client_port_attempt_time",
		"user_auth_attempt_pkey",
		"user_auth_attempt_user_auth",
		"user_auth_reset_pkey",
		"user_auth_reset_reset_code_key",
		"user_auth_reset_user_id",
		"user_auth_verify_pkey",
		"user_auth_verify_user_id_verify_code_key",
		"pg_toast_16944_index",
		"location_pkey",
		"location_location_full_name_key",
		"location_type_country_code_name",
		"location_group_pkey",
		"location_group_location_group_name_key",
		"circle_uc_pkey",
		"auth_code_active_network_id",
		"auth_code_active_user_id",
		"auth_code_auth_code_key",
		"auth_code_end_time",
		"auth_code_pkey",
		"pg_toast_8252216_index",
		"auth_code_session_pkey",
		"auth_session_active_network_id",
		"auth_session_active_user_id",
		"auth_session_pkey",
		"auth_session_expiration_pkey",
		"device_pkey",
		"device_association_name_pkey",
		"device_add_history_pkey",
		"pg_toast_11853493_index",
		"device_association_code_code_key",
		"device_association_code_pkey",
		"device_association_code_type",
		"pg_toast_11853501_index",
		"device_adopt_pkey",
		"device_adopt_owner_network_id",
		"pg_toast_11853511_index",
		"device_adopt_auth_session_pkey",
		"device_share_guest_network_id",
		"device_share_pkey",
		"device_share_source_network_id",
		"pg_toast_11853482_index",
		"pending_task_has_reschedule_error",
		"pending_task_pkey",
		"pending_task_run_at_block",
		"pending_task_run_once_key_key",
		"finished_task_pkey",
		"finished_task_run_end_time",
		"pg_toast_13339766_index",
		"network_connection_reliability_window_pkey",
		"network_client_location_reliability_pkey",
		"network_client_location_reliability_update_block_number_client_",
		"network_client_location_reliability_valid_connected_client_id",
		"network_client_location_reliability_multiplier_pkey",
		"subscription_payment_network_id_create_time",
		"subscription_payment_pkey",
		"network_client_latency_pkey",
		"network_client_speed_pkey",
		"solana_payment_intent_payment_reference",
		"solana_payment_intent_pkey",
		"solana_payment_intent_tx_signature",
		"pg_toast_339089448_index",
		"stripe_customer_network_id_unique",
		"stripe_customer_pkey",
		"stripe_customer_stripe_customer_id_key",
		"search_value_update_pkey",
		"search_value_update_realm_value_id_variant",
		"pg_toast_1092242711_index",
		"network_referral_code_pkey",
		"network_referral_code_referral_code_key",
		"network_client_handler_pkey",
		"network_client_handler_heartbeat_time",
		"audit_account_payment_pkey",
		"pg_toast_1620447681_index",
		"pg_toast_16969_index",
		"subsidy_payment_end_time_start_time",
		"subsidy_payment_pkey",
		"subscription_renewal_pkey",
		"pg_toast_1630729945_index",
		"pg_toast_16931_index",
		"network_referral_pkey",
		"network_point_pkey",
		"payment_report_pkey",
		"pg_toast_16996_index",
		"pg_toast_13339752_index",
		"exclude_network_client_location_client_location_id_network_id",
		"exclude_network_client_location_pkey",
		"network_user_auth_sso_pkey",
		"network_user_auth_sso_product_updates_sync_user_id",
		"network_user_auth_sso_user_auth_auth_type_key",
		"pg_toast_4031129753_index",
		"network_user_auth_password_product_updates_sync_user_id",
		"network_user_auth_password_pkey",
		"network_user_auth_password_user_auth_auth_type_key",
		"pg_toast_4031129778_index",
		"network_user_auth_wallet_pkey",
		"network_user_auth_wallet_wallet_address_blockchain_key",
		"pg_toast_4031129789_index",
		"client_reliability_valid_block_number_client_address_hash",
		"client_reliability_pkey",
		"pg_toast_4131663093_index",
		"client_connection_reliability_score_pkey",
		"network_connection_reliability_score_pkey",
		"provide_key_change_pkey",
	}

	for _, indexName := range incompleteIndexNames {
		connect.AssertEqual(t, isIncompleteIndexName(indexName), true)
	}
	for _, indexName := range completeIndexNames {
		connect.AssertEqual(t, isIncompleteIndexName(indexName), false)
	}
}
