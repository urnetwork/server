# Historical trace-retry branch

Generic handler panic retry is not coupled to current side-effect idempotency or cancellation; replaying arbitrary handlers would bypass current owning transaction retry boundaries.

The exact original unique commits are preserved as patches and as ancestry of this merge. This directory is non-built historical material; no route, retry, schema migration or deployment workflow is activated. Current owning implementations and controls are retained.
