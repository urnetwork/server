# Historical google-sub-fix branch

Historical debug logging emits purchase tokens/webhook bodies. Current subscriptions-v2 reconciliation and playAcknowledgeSubscription already preserve acknowledgement failures with retry semantics; preserve current implementation and do not add new raw-payload diagnostics.

The original patches are retained exactly for review. This archive is outside active Go packages and CI workflow paths. The branch head is a merge parent; this is historical source preservation, not deployment or validation of the obsolete implementation.
