package server

// A report is scoped to the existing contract/party owner. Its receipt and the
// incremental close update commit together; contract deletion reclaims both.
const contractCloseReportSchemaSql = `
CREATE TABLE contract_close_report (
 contract_id uuid NOT NULL REFERENCES transfer_contract(contract_id) ON DELETE CASCADE,
 party text NOT NULL CHECK (party IN ('source','destination')),
 report_id uuid NOT NULL CHECK (report_id <> '00000000-0000-0000-0000-000000000000'::uuid),
 used_transfer_byte_count bigint NOT NULL CHECK (used_transfer_byte_count >= 0),
 checkpoint boolean NOT NULL,
 create_time timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(contract_id,party,report_id)
);
`
