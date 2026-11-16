// The companion origin reads as they were before the lingering origins' side
// table (contract_origin_linger.go), kept verbatim. A closed origin arm here
// reads the closed pair range of transfer_contract_open_source_id. Each read
// uses these only until the side table's state row is complete, and they go
// with the index's drop record (OPEN-SOURCE-ID-RETIRE.md §4, step 6): without
// the index each would read a destination's recent fan-in or every recent
// contract.
package model

// companionOriginExists's statement.
var legacyCompanionOriginExistsSql = `
	SELECT EXISTS (
		SELECT 1 FROM transfer_contract
		WHERE
			-- equivalent to the generated open flag but opaque to legacy
			-- false-zero open/outcome indexes
			(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
			-- ids lead with their creation millisecond: an unexpired
			-- contract's id is at least this bound
			contract_id >= ` + unexpiredContractIdFloorSql + ` AND
			COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
			source_id = $1 AND
			destination_id = $2
	) OR EXISTS (
		SELECT 1 FROM transfer_contract
		WHERE
			open = false AND
			$3 <= close_time AND
			-- ids lead with their creation millisecond: an unexpired
			-- contract's id is at least this bound
			contract_id >= ` + unexpiredContractIdFloorSql + ` AND
			COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
			source_id = $1 AND
			destination_id = $2
	)
`

// createCompanionTransferEscrow's plain origin selection.
var legacyCompanionPlainOriginSql = `
                SELECT contract_id,
                    CASE WHEN transfer_byte_count >= $5::bigint THEN NULL
                    WHEN EXISTS (SELECT 1 FROM prober_identity WHERE singleton AND network_id = $4
                        UNION ALL SELECT 1 FROM prober_shard_run WHERE network_id = $4)
                    THEN GREATEST(transfer_byte_count, (
                        SELECT max(transfer_byte_count)
                        FROM (
                            SELECT transfer_byte_count FROM transfer_contract
                            WHERE
                                (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
                                contract_id >= ` + unexpiredContractIdFloorSql + ` AND
                                COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                source_id = $1 AND destination_id = $2 AND
                                companion_contract_id IS NULL
                            UNION ALL
                            SELECT transfer_byte_count FROM transfer_contract
                            WHERE open = false AND $3 <= close_time AND
                                contract_id >= ` + unexpiredContractIdFloorSql + ` AND
                                COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                source_id = $1 AND destination_id = $2 AND
                                companion_contract_id IS NULL
                        ) AS eligible_probe_origins
                    )) END AS prober_reservation_byte_count
                FROM (
                    (
                        SELECT contract_id, create_time, transfer_byte_count
                        FROM transfer_contract
                        WHERE
							-- The CASE is equivalent to the generated open flag but
							-- opaque to legacy false-zero open/outcome indexes.
							(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
                            -- ids lead with their creation millisecond: an
                            -- unexpired contract's id is at least this bound
                            contract_id >= ` + unexpiredContractIdFloorSql + ` AND
                            COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                            source_id = $1 AND
                            destination_id = $2 AND
                            companion_contract_id IS NULL
                        ORDER BY create_time ASC
                        LIMIT 1
                    )

                    UNION ALL

                    (
                        SELECT contract_id, create_time, transfer_byte_count
                        FROM (
                            SELECT contract_id, create_time, transfer_byte_count
                            FROM transfer_contract
                            WHERE
                                open = false AND
                                $3 <= close_time AND
                                -- ids lead with their creation millisecond: an
                                -- unexpired contract's id is at least this bound
                                contract_id >= ` + unexpiredContractIdFloorSql + ` AND
                                COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                source_id = $1 AND
                                destination_id = $2 AND
                                companion_contract_id IS NULL
                            -- read the bounded pair range whole: no ordered
                            -- scan of another index may serve the LIMIT
                            OFFSET 0
                        ) AS closed_origin
                        ORDER BY create_time ASC
                        LIMIT 1
                    )

                    -- the two branches are disjoint on open, so the global
                    -- earliest is the earlier of each branch's earliest; each
                    -- inner query is a bounded index range (the open pair
                    -- partial indexes, and transfer_contract_open_source_id
                    -- bounded by the contract id), which keeps the planner off
                    -- the create_time full scan
                    ORDER BY create_time ASC
                    LIMIT 1
                ) AS earliest_origin
            `

// createCompanionTransferEscrow's chained origin selection.
var legacyCompanionChainedOriginSql = `
                    SELECT contract_id,
                        CASE WHEN transfer_byte_count >= $7::bigint THEN NULL
                        WHEN EXISTS (SELECT 1 FROM prober_identity WHERE singleton AND network_id = $4
                            UNION ALL SELECT 1 FROM prober_shard_run WHERE network_id = $4)
                        THEN GREATEST(transfer_byte_count, (
                            SELECT max(transfer_byte_count)
                            FROM (
                                SELECT transfer_byte_count, payer_network_id,
                                    source_network_id, destination_network_id
                                FROM transfer_contract
                                WHERE
                                    (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
                                    contract_id >= ` + unexpiredContractIdFloorSql + ` AND
                                    COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                    source_id = $1 AND destination_id = $2 AND
                                    companion_contract_id IS NOT NULL
                                UNION ALL
                                SELECT transfer_byte_count, payer_network_id,
                                    source_network_id, destination_network_id
                                FROM transfer_contract
                                WHERE open = false AND $3 <= close_time AND
                                    contract_id >= ` + unexpiredContractIdFloorSql + ` AND
                                    COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                    source_id = $1 AND destination_id = $2 AND
                                    companion_contract_id IS NOT NULL
                                -- Filter private ownership outside the pair boundary;
                                -- false-zero stats must not substitute a payer scan.
                                OFFSET 0
                            ) AS eligible_probe_companion_origins
                            WHERE ($4::uuid <> $5 OR (payer_network_id = $4 AND
                                source_network_id = $6 AND destination_network_id = $5))
                        )) END AS prober_reservation_byte_count,
                        CASE WHEN payer_network_id = $5 AND source_network_id = $6
                            AND destination_network_id = $5
                            AND EXISTS (SELECT 1 FROM prober_shard_run WHERE network_id = $5)
                        THEN $5::uuid END AS inherited_private_payer
                    FROM (
                        (
                            SELECT contract_id, create_time, transfer_byte_count,
                                payer_network_id, source_network_id, destination_network_id
                            FROM transfer_contract
                            WHERE
								-- Keep both generic open and outcome-null partial
								-- indexes ineligible for this pair lookup.
								(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
                                -- ids lead with their creation millisecond: an
                                -- unexpired contract's id is at least this bound
                                contract_id >= ` + unexpiredContractIdFloorSql + ` AND
                                COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                source_id = $1 AND
                                destination_id = $2 AND
                                companion_contract_id IS NOT NULL
                            ORDER BY create_time ASC
                            LIMIT 1
                        )

                        UNION ALL

                        (
                            SELECT contract_id, create_time, transfer_byte_count,
                                payer_network_id, source_network_id, destination_network_id
                            FROM (
                                SELECT contract_id, create_time, transfer_byte_count,
                                    payer_network_id, source_network_id, destination_network_id
                                FROM transfer_contract
                                WHERE
                                    open = false AND
                                    $3 <= close_time AND
                                    contract_id >= ` + unexpiredContractIdFloorSql + ` AND
                                    COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                    source_id = $1 AND
                                    destination_id = $2 AND
                                    companion_contract_id IS NOT NULL
                                OFFSET 0
                            ) AS closed_companion_origin
                            ORDER BY create_time ASC
                            LIMIT 1
                        )

                        ORDER BY create_time ASC
                        LIMIT 1
                    ) AS earliest_companion_origin
                `

// zeroEscrowCompanionOriginInTx's plain origin read.
var legacyZeroEscrowPlainOriginSql = `
	SELECT contract_id
	FROM (
		(
			SELECT contract_id, create_time
			FROM transfer_contract
			WHERE
				-- equivalent to the generated open flag but opaque to legacy
				-- false-zero open/outcome indexes
				(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
				-- ids lead with their creation millisecond: an unexpired
				-- contract's id is at least this bound
				contract_id >= ` + unexpiredContractIdFloorSql + ` AND
				COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
				source_id = $1 AND
				destination_id = $2 AND
				companion_contract_id IS NULL
			ORDER BY create_time ASC
			LIMIT 1
		)

		UNION ALL

		(
			SELECT contract_id, create_time
			FROM (
				SELECT contract_id, create_time
				FROM transfer_contract
				WHERE
					open = false AND
					$3 <= close_time AND
					contract_id >= ` + unexpiredContractIdFloorSql + ` AND
					COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
					source_id = $1 AND
					destination_id = $2 AND
					companion_contract_id IS NULL
				OFFSET 0
			) AS closed_origin
			ORDER BY create_time ASC
			LIMIT 1
		)

		ORDER BY create_time ASC
		LIMIT 1
	) AS earliest_origin
`

// zeroEscrowCompanionOriginInTx's chained origin read.
var legacyZeroEscrowChainedOriginSql = `
	SELECT
		contract_id,
		CASE WHEN (payer_network_id IS NULL OR payer_network_id = $4) AND
			source_network_id = $5 AND
			destination_network_id = $4 AND
			EXISTS (SELECT 1 FROM prober_shard_run WHERE network_id = $4)
		THEN $4::uuid END AS inherited_private_payer
	FROM (
		(
			SELECT contract_id, create_time, payer_network_id, source_network_id, destination_network_id
			FROM transfer_contract
			WHERE
				-- equivalent to the generated open flag but opaque to legacy
				-- false-zero open/outcome indexes
				(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
				-- ids lead with their creation millisecond: an unexpired
				-- contract's id is at least this bound
				contract_id >= ` + unexpiredContractIdFloorSql + ` AND
				COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
				source_id = $1 AND
				destination_id = $2 AND
				companion_contract_id IS NOT NULL
			ORDER BY create_time ASC
			LIMIT 1
		)

		UNION ALL

		(
			SELECT contract_id, create_time, payer_network_id, source_network_id, destination_network_id
			FROM (
				SELECT contract_id, create_time, payer_network_id, source_network_id, destination_network_id
				FROM transfer_contract
				WHERE
					open = false AND
					$3 <= close_time AND
					contract_id >= ` + unexpiredContractIdFloorSql + ` AND
					COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
					source_id = $1 AND
					destination_id = $2 AND
					companion_contract_id IS NOT NULL
				OFFSET 0
			) AS closed_companion_origin
			ORDER BY create_time ASC
			LIMIT 1
		)

		ORDER BY create_time ASC
		LIMIT 1
	) AS earliest_companion_origin
`
