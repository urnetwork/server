package server

// Durable payout planning state. A plan commits its financial decisions
// (payments, subsidy epoch, points) first and assigns its selected sweeps in
// bounded transactions afterwards, so a drain or restart resumes the
// assignment instead of rolling the whole slice back.
//
// payment_plan_assignment holds at most one pending plan (complete_time IS
// NULL). Its payments are not payable until every staged sweep is assigned.
// payment_plan_sweep reserves each staged sweep for exactly one plan payment:
// the owner guard refuses any other writer (including an older binary's
// planner) that would move a reserved sweep to a different payment.
//
// The guard locks transfer_escrow_sweep against writes while it is created.
// The lock timeout keeps it from queueing settlement's sweep inserts behind a
// long lock holder (a payout plan's assignment, or the nightly pg_dump); a
// migrate that times out is re-run.
const paymentPlanAssignmentSchemaSql = `
SET LOCAL lock_timeout = '5s';

CREATE TABLE payment_plan_assignment (
    payment_plan_id uuid NOT NULL PRIMARY KEY,
    create_time timestamp NOT NULL,
    sweep_count bigint NOT NULL CHECK (0 <= sweep_count),
    assigned_sweep_count bigint NOT NULL DEFAULT 0 CHECK (0 <= assigned_sweep_count AND assigned_sweep_count <= sweep_count),
    cursor_contract_id uuid NULL,
    cursor_balance_id uuid NULL,
    cursor_network_id uuid NULL,
    complete_time timestamp NULL
);
CREATE UNIQUE INDEX payment_plan_assignment_pending ON payment_plan_assignment ((true)) WHERE complete_time IS NULL;

CREATE TABLE payment_plan_sweep (
    contract_id uuid NOT NULL,
    balance_id uuid NOT NULL,
    network_id uuid NOT NULL,
    payment_plan_id uuid NOT NULL REFERENCES payment_plan_assignment (payment_plan_id),
    previous_payment_id uuid NULL,
    payment_id uuid NOT NULL,
    PRIMARY KEY (contract_id, balance_id, network_id)
);

CREATE FUNCTION payment_plan_sweep_owner_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$
BEGIN
    IF NEW.payment_id IS DISTINCT FROM OLD.payment_id AND EXISTS (
        SELECT 1 FROM payment_plan_sweep staged
        WHERE staged.contract_id = OLD.contract_id AND staged.balance_id = OLD.balance_id AND
            staged.network_id = OLD.network_id AND staged.payment_id IS DISTINCT FROM NEW.payment_id
    ) THEN
        RAISE EXCEPTION 'transfer escrow sweep is reserved by a pending payment plan';
    END IF;
    RETURN NEW;
END $guard$;
CREATE TRIGGER payment_plan_sweep_owner_guard BEFORE UPDATE OF payment_id ON transfer_escrow_sweep
    FOR EACH ROW EXECUTE FUNCTION payment_plan_sweep_owner_guard();
`
