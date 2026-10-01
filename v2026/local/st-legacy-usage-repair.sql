-- Explicit operator repair only. The caller supplies exact manifest bytes and
-- their independently reviewed sha256 in transaction-local settings, inside a
-- serializable transaction. No automatic epoch reader invokes this procedure.
-- Every original contract and report remains unchanged; only a missing usage
-- snapshot becomes zero-credit debt. The receipt hash is over the input bytes,
-- so neither the manifest nor its hash contains the resulting snapshot.
DO $st_legacy_usage_repair$
DECLARE
    wire text := current_setting('urnetwork.st_usage_repair_manifest');
    manifest jsonb := wire::jsonb;
    manifest_hash text := 'sha256:' || encode(sha256(convert_to(wire, 'UTF8')), 'hex');
    expected_count bigint := (manifest->>'contract_count')::bigint;
    expected_debt bigint := (manifest->>'retained_report_minimum')::bigint;
    epoch_start timestamp := (manifest->>'epoch_start')::timestamp;
    epoch_end timestamp := (manifest->>'epoch_end')::timestamp;
    legacy_start timestamp := (manifest->>'legacy_start')::timestamp;
    legacy_end timestamp := (manifest->>'legacy_end')::timestamp;
    affected bigint;
BEGIN
    IF current_setting('transaction_isolation') <> 'serializable'
        OR manifest_hash <> current_setting('urnetwork.st_usage_repair_sha256')
        OR manifest->>'schema' IS DISTINCT FROM 'urnetwork-st-legacy-usage-repair-v1'
        OR manifest->'final_acceptance' IS DISTINCT FROM 'false'::jsonb
        OR (manifest->>'epoch')::bigint IS NULL OR (manifest->>'epoch')::bigint <= 0
        OR expected_count IS NULL OR expected_count <= 0
        OR expected_debt IS NULL OR expected_debt < 0
        OR epoch_start IS NULL OR epoch_end IS NULL OR epoch_start >= epoch_end
        OR legacy_start IS NULL OR legacy_end IS NULL OR legacy_start >= legacy_end
        OR legacy_start < epoch_start OR legacy_end > epoch_end
        OR jsonb_typeof(manifest->'contracts') IS DISTINCT FROM 'array'
        OR jsonb_array_length(manifest->'contracts') <> expected_count
    THEN RAISE EXCEPTION 'legacy usage repair manifest identity, scope or digest differs'; END IF;

    CREATE TEMP TABLE st_legacy_usage_repair_expected (
        contract_id uuid PRIMARY KEY,
        original jsonb NOT NULL,
        debt bigint NOT NULL,
        snapshot jsonb NOT NULL
    ) ON COMMIT DROP;
    INSERT INTO st_legacy_usage_repair_expected
    SELECT (item->'contract'->>'contract_id')::uuid, item,
        (SELECT min((report->>'used_transfer_byte_count')::bigint)
         FROM jsonb_array_elements(item->'closes') AS report),
        jsonb_build_object('version', 1, 'byte_count', 0, 'providers', '[]'::jsonb,
            'excluded_reason', 'legacy_usage_unavailable', 'legacy_exclusion',
            jsonb_build_object('repair_manifest_sha256', manifest_hash,
                'contract_id', item->'contract'->>'contract_id', 'epoch', (manifest->>'epoch')::bigint,
                'closed_at', to_char((item->'contract'->>'close_time')::timestamp, 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
                'retained_report_minimum', (SELECT min((report->>'used_transfer_byte_count')::bigint)
                    FROM jsonb_array_elements(item->'closes') AS report), 'final_acceptance', false))
    FROM jsonb_array_elements(manifest->'contracts') AS item;

    IF (SELECT count(*) FROM st_legacy_usage_repair_expected) <> expected_count
        OR (SELECT sum(debt) FROM st_legacy_usage_repair_expected) <> expected_debt
        OR EXISTS (SELECT 1 FROM st_legacy_usage_repair_expected AS e
            WHERE e.original->'contract'->>'outcome' IS DISTINCT FROM 'settled'
                OR e.original->'contract'->'open' IS DISTINCT FROM 'false'::jsonb
                OR e.original->'contract'->'provider_usage' IS DISTINCT FROM 'null'::jsonb
                OR jsonb_typeof(e.original->'contract'->'usage_origin_is_source') IS DISTINCT FROM 'boolean'
                OR e.original->'contract'->'usage_unverified' IS DISTINCT FROM 'false'::jsonb
                OR jsonb_typeof(e.original->'contract'->'close_time') IS DISTINCT FROM 'string'
                OR (e.original->'contract'->>'close_time')::timestamp < legacy_start
                OR (e.original->'contract'->>'close_time')::timestamp >= legacy_end
                OR jsonb_array_length(e.original->'closes') <> 2
                OR (SELECT array_agg(report->>'party' ORDER BY report->>'party')
                    FROM jsonb_array_elements(e.original->'closes') AS report) <> ARRAY['destination','source']
                OR EXISTS (SELECT 1 FROM jsonb_array_elements(e.original->'closes') AS report
                    WHERE report->>'contract_id' IS DISTINCT FROM e.contract_id::text
                        OR report->'checkpoint' IS DISTINCT FROM 'false'::jsonb
                        OR jsonb_typeof(report->'used_transfer_byte_count') IS DISTINCT FROM 'number'
                        OR (report->>'used_transfer_byte_count')::bigint < 0)
                OR e.debt < 0)
    THEN RAISE EXCEPTION 'legacy usage repair cohort or retained debt differs'; END IF;

    -- Terminal owner first, then its exact reports, in deterministic order.
    PERFORM c.contract_id FROM transfer_contract AS c
        JOIN st_legacy_usage_repair_expected AS e USING (contract_id)
        ORDER BY c.contract_id FOR UPDATE OF c;
    PERFORM c.contract_id FROM contract_close AS c
        JOIN st_legacy_usage_repair_expected AS e USING (contract_id)
        ORDER BY c.contract_id, c.party FOR UPDATE OF c;
    IF (SELECT count(*) FROM transfer_contract AS c
            JOIN st_legacy_usage_repair_expected AS e USING (contract_id)) <> expected_count
        OR (SELECT count(*) FROM transfer_contract WHERE epoch_start <= close_time AND close_time < epoch_end
            AND outcome IN ('settled','dispute_resolved_to_source','dispute_resolved_to_destination')
            AND provider_usage IS NULL) <> expected_count
        OR EXISTS (SELECT 1 FROM st_legacy_usage_repair_expected AS e
            JOIN transfer_contract AS c USING (contract_id)
            WHERE c.provider_usage IS NOT NULL OR to_jsonb(c) <> e.original->'contract'
                OR COALESCE((SELECT jsonb_agg(to_jsonb(report) ORDER BY report.party)
                    FROM contract_close AS report WHERE report.contract_id=e.contract_id), '[]'::jsonb) <> e.original->'closes')
    THEN RAISE EXCEPTION 'legacy usage repair original rows changed or census differs'; END IF;

    UPDATE transfer_contract AS c SET provider_usage=e.snapshot
        FROM st_legacy_usage_repair_expected AS e
        WHERE c.contract_id=e.contract_id AND c.provider_usage IS NULL;
    GET DIAGNOSTICS affected = ROW_COUNT;
    IF affected <> expected_count OR EXISTS (
        SELECT 1 FROM transfer_contract AS c JOIN st_legacy_usage_repair_expected AS e USING (contract_id)
        WHERE c.provider_usage <> e.snapshot OR (to_jsonb(c)-'provider_usage') <> ((e.original->'contract')-'provider_usage'))
    THEN RAISE EXCEPTION 'legacy usage repair write differs from exact zero-credit cohort'; END IF;

    -- The same digest and debt persist in every repaired snapshot. This session
    -- receipt survives successful commit; rollback restores the prior setting.
    PERFORM set_config('urnetwork.st_usage_repair_receipt', jsonb_build_object(
        'schema', 'urnetwork-st-legacy-usage-repair-receipt-v1',
        'repair_manifest_sha256', manifest_hash, 'epoch', (manifest->>'epoch')::bigint,
        'contracts', affected, 'retained_report_minimum', expected_debt,
        'credited_bytes', 0, 'final_acceptance', false,
        'transaction_id', txid_current(), 'applied_at', clock_timestamp())::text, false);
END
$st_legacy_usage_repair$;
