# Historical multi-auth-guest branch

Obsolete destructive auth-column migration conflicts with current mixed-version/adoption/migration readers and canonical schema765. Current HasAnyAuthMethod queries credential child tables and current authenticated account operations retain compatibility fields.

The original patches are retained exactly for review. This archive is outside active Go packages and CI workflow paths. The branch head is a merge parent; this is historical source preservation, not deployment or validation of the obsolete implementation.
