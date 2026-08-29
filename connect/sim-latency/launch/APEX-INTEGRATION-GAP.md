# Apex external-evaluator integration record

Status date: 2026-08-29

The UR competition control plane and evaluator protocol are implemented for a
dedicated external scoring service. Macrocosmos has approved this non-standard
external-evaluator path. It is deliberately not a drop-in deployment of the
current public Apex runner contract.

The concrete asynchronous adapter mapping and external acceptance fields are
prepared in [`APEX-HANDOFF.md`](APEX-HANDOFF.md).

## Current public Apex contract

The current
[`apex-competitions-builder`](https://github.com/macrocosm-os/apex-competitions-builder)
defines a competition as an `apex.competition.v1` spec plus digest-pinned,
keyless-cosign-signed player and referee images. A solo submission runs in a
player sandbox and is driven over a per-job network by a separate referee
sandbox. The public schema has no external generate/submit/poll scoring-service
adapter.

The same schema currently caps each production sandbox at four CPUs and 4 GiB,
and caps player/referee timeouts at 7,200 seconds. The sim-latency evaluator
requires a qualified 12-CPU/128-GB host class, approximately 13 GiB for the
simulator at the present frontier point, dedicated PostgreSQL and Redis in the
same resource boundary, and a paired multi-replicate baseline/candidate job.
Those requirements do not fit the standard public sandbox contract.

The season contract accepts an unbounded number of $20 USD submissions for
seven days and evaluates them immediately through one Redis-list FIFO backed by
durable PostgreSQL ordering. One calibrated job has a three-hour hard execution
limit. At close the backlog keeps running, and the external control loop begins
the next of six epochs only after the prior FIFO drains, the one-shot worker
exits, and ordered honesty review finalizes an approved winner or exhausts the
candidate list. Only then do embargoed results publish. A synchronous
solo-referee result cannot preserve that timing or cache identity without an
accepted asynchronous adapter.

The standard solo leaderboard also applies a one-percent takeover rule. This
competition deliberately freezes its takeover margin only after same-seed,
independent-seed, and reference-patch calibration; adopting one percent before
that evidence would violate the score contract.

Finally, the Apex registry is private. The public onboarding flow asks the
competition owner to publish a separate competition repository, released
`spec.yaml`, signed image digests, and a completed `HANDOFF.md`; a Macrocosmos
maintainer then copies the reviewed spec into the private registry and activates
stage before production. There is no public registry pull request to create.

## Approved integration path

Macrocosmos has approved an external-evaluator adapter that maps Apex
submissions to the authenticated `/competition/score` and
`/competition/score/{jobId}` API, preserves canonical patch bytes and hidden
round identity, and honors the calibrated significance/takeover decision
returned by the service.

The alternative dedicated-host referee profile is not required unless Apex and
UR later replace the approved adapter design.

The remaining work is operational: assign the runnable integration owner;
record its stage/production release identity and private-registry entry; issue
stage credentials; prove retry/cache idempotency and one complete staging
submission; and record economics, participant policy, incident contacts, and
any standard-artifact waivers. The current checklist is
[`APEX-OPEN-QUESTIONS.md`](APEX-OPEN-QUESTIONS.md).

The REST/OpenAPI package is ready for integration. The Apex stage run, release
identity, registry activation, completed onboarding record, and any artifacts
or waivers required for the approved external-evaluator path remain external
deliverables.
