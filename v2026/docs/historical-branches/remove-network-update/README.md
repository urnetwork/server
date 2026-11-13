# Historical remove-network-update branch

Historical network_user.network_id backfill at old schema position and unfenced deletion conflict with current canonical765, admin authorization, payment row-lock and active Stripe renewal refusal. Current deletion already clears all child auth methods.

The original patches are retained exactly for review. This archive is outside active Go packages and CI workflow paths. The branch head is a merge parent; this is historical source preservation, not deployment or validation of the obsolete implementation.
