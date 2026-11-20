-- Fixed sparse/dense work control: 660008 contracts and 1520014 reports.
-- Temporary relations isolate this planner test from migrated production tables.
SET statement_timeout='150s';
SET search_path=pg_temp,public;
CREATE TEMP TABLE transfer_contract (
 contract_id uuid PRIMARY KEY,
 destination_id uuid NOT NULL,
 source_id uuid NOT NULL,
 source_network_id uuid NOT NULL,
 destination_network_id uuid NOT NULL,
 payer_network_id uuid NOT NULL,
 transfer_byte_count bigint NOT NULL,
 create_time timestamp NOT NULL,
 close_time timestamp,
 outcome varchar(32),
 fixture_payload varchar(128)
) WITH(autovacuum_enabled=false);
CREATE INDEX transfer_contract_destination_id_close_time ON transfer_contract(destination_id,close_time);
CREATE INDEX transfer_contract_closed_usage ON transfer_contract(close_time,contract_id) WHERE outcome IS NOT NULL;
CREATE INDEX transfer_contract_outcome_null ON transfer_contract(contract_id) WHERE outcome IS NULL;
CREATE TEMP TABLE contract_close (
 contract_id uuid NOT NULL,party varchar(16) NOT NULL,
 used_transfer_byte_count bigint,close_time timestamp,
 PRIMARY KEY(contract_id,party)
) WITH(autovacuum_enabled=false);
INSERT INTO transfer_contract
SELECT lpad(to_hex(g),32,'0')::uuid,lpad(to_hex(g%200000+1),32,'0')::uuid,
 lpad(to_hex(g%150000+1),32,'0')::uuid,lpad('1',32,'0')::uuid,
 lpad('2',32,'0')::uuid,lpad('3',32,'0')::uuid,7,timestamp '2026-01-01',
 CASE WHEN g<=40000 THEN timestamp '2026-10-06'
      WHEN g<=600000 THEN timestamp '2026-09-01'
      WHEN g<=620000 THEN NULL
      WHEN g<=620100 THEN timestamp '2026-10-06'
      WHEN g<=640000 THEN timestamp '2026-09-01'
      ELSE timestamp '2026-10-08' END,
 CASE WHEN g>600000 AND g<=640000 THEN NULL WHEN g>640000 THEN 'canceled' ELSE 'settled' END,
 repeat('synthetic-width-',8)
FROM generate_series(1,660000)g;
INSERT INTO contract_close
SELECT contract_id,party,
 CASE WHEN party='source' THEN 999 WHEN contract_id=lpad('4',32,'0')::uuid THEN NULL ELSE 7 END,
 timestamp '2026-08-01'
FROM transfer_contract CROSS JOIN (VALUES('source'),('destination'))p(party)
WHERE NOT(contract_id=lpad('3',32,'0')::uuid AND party='destination');
-- Unreferenced report history must not enter the daily aggregate.
INSERT INTO contract_close
SELECT lpad(to_hex(1000000+g),32,'0')::uuid,'destination',555,timestamp '2026-10-06'
FROM generate_series(1,200000)g;
-- These expose unsafe predicate changes: null outcome, half-open day edges,
-- canceled outcome, missing report, null bytes and independent report clocks.
INSERT INTO transfer_contract
SELECT lpad(to_hex(800000+ordinal),32,'0')::uuid,lpad('1',32,'0')::uuid,
 lpad('1',32,'0')::uuid,lpad('1',32,'0')::uuid,lpad('2',32,'0')::uuid,
 lpad('3',32,'0')::uuid,7,timestamp '2026-01-01',closed,outcome,'semantic-control'
FROM (VALUES
 (1,timestamp '2026-10-06',NULL::varchar),
 (2,timestamp '2026-10-07','settled'),
 (3,timestamp '2026-10-06'-interval '1 microsecond','settled'),
 (4,timestamp '2026-10-07'-interval '1 microsecond','canceled'),
 (5,timestamp '2026-10-06'+interval '1 hour','settled'),
 (6,timestamp '2026-10-06'+interval '2 hours','settled'),
 (7,NULL,NULL),
 (8,timestamp '2026-10-06'+interval '3 hours','settled')
)s(ordinal,closed,outcome);
INSERT INTO contract_close
SELECT lpad(to_hex(800000+ordinal),32,'0')::uuid,'source',999,timestamp '2026-10-06'
FROM generate_series(1,8)ordinal;
INSERT INTO contract_close
SELECT lpad(to_hex(800000+ordinal),32,'0')::uuid,'destination',bytes,timestamp '2026-08-01'
FROM (VALUES(1,11::bigint),(2,13),(3,17),(4,19),(6,NULL),(7,23),(8,0))s(ordinal,bytes);
ANALYZE transfer_contract,contract_close;
