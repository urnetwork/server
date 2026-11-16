package server

import "strings"

// Migration 755 installs compatibility first. Activation is a separate operator
// action after every creator, settler and maintenance reader is upgraded.
// Existing rows remain exact legacy reservations. New Redis reservations never
// update a shared PostgreSQL revision or snapshot row.
var NetEscrowRowsRedisRevisionFunctionBodySql = strings.ReplaceAll(NetEscrowRowsRevisionFunctionBodySql,
	"WHERE NOT settled AND balance_byte_count <> 0", "WHERE NOT settled AND balance_byte_count <> 0 AND NOT redis_reserved")

var NetEscrowContractsRedisRevisionFunctionBodySql = strings.ReplaceAll(strings.ReplaceAll(NetEscrowContractsRevisionFunctionBodySql,
	"SELECT balance_id, settled, balance_byte_count FROM transfer_escrow", "SELECT balance_id, settled, balance_byte_count, redis_reserved FROM transfer_escrow"),
	"AND escrow.balance_byte_count <> 0", "AND escrow.balance_byte_count <> 0 AND NOT escrow.redis_reserved")

var redisContractAdmissionSchemaSql = `
    ALTER TABLE transfer_escrow ADD COLUMN redis_reserved boolean NOT NULL DEFAULT false;
    CREATE TABLE redis_contract_admission_policy (
        singleton boolean PRIMARY KEY CHECK (singleton),
        enabled boolean NOT NULL DEFAULT false
    );
    INSERT INTO redis_contract_admission_policy(singleton,enabled) VALUES(true,false);
    CREATE OR REPLACE FUNCTION transfer_escrow_revision()
    RETURNS trigger LANGUAGE plpgsql AS $escrow$` + NetEscrowRowsRedisRevisionFunctionBodySql + `$escrow$;
    CREATE OR REPLACE FUNCTION transfer_contract_escrow_revision()
    RETURNS trigger LANGUAGE plpgsql AS $contract$` + NetEscrowContractsRedisRevisionFunctionBodySql + `$contract$;
`
