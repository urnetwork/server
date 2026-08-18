# Apex integration decision record

Status date: 2026-08-16

The UR competition control plane and evaluator protocol are implemented for a
dedicated external scoring service. They cannot truthfully be described as a
drop-in deployment of the current public Apex runner contract without a
Macrocosmos integration decision.

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

The standard solo leaderboard also applies a one-percent takeover rule. This
competition deliberately freezes its takeover margin only after same-seed,
independent-seed, and reference-patch calibration; adopting one percent before
that evidence would violate the score contract.

Finally, the Apex registry is private. The public onboarding flow asks the
competition owner to publish a separate competition repository, released
`spec.yaml`, signed image digests, and a completed `HANDOFF.md`; a Macrocosmos
maintainer then copies the reviewed spec into the private registry and activates
stage before production. There is no public registry pull request to create.

## Decision required before staging

Macrocosmos must approve one of these paths:

1. an external-evaluator adapter that maps Apex submissions to the authenticated
   `/competition/score` and `/competition/score/{jobId}` API, preserves the
   canonical patch bytes and hidden-round identity, and honors the calibrated
   takeover decision returned by this service; or
2. a negotiated dedicated-host referee profile large enough to run the complete
   evaluator boundary, with a reviewed custom protocol and resource ceilings.

The chosen path must also define submission-fee handling, round and reveal
timing, maximum patch payload, polling deadline, result/artifact disclosure,
cosign identity, stage credentials, and who owns retries and cache identity.

Until that decision is recorded, the REST/OpenAPI package is ready for local
and UR staging integration, but an `apex.competition.v1` spec, signed player and
referee image digests, Apex stage run, registry activation, and accepted
`HANDOFF.md` remain blocked external deliverables.
