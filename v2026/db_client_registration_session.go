// Replay authority belongs to the original registration, even after a rebind.
package server

// Historical rows deliberately retain null evidence. A current client binding
// cannot establish which session an older registration originally authorized.
const clientRegistrationSessionSchemaSql = `
SET LOCAL lock_timeout = '5s';
ALTER TABLE network_client_registration
	ADD COLUMN session_id uuid NULL,
	ADD COLUMN session_create_time timestamp NULL,
	ADD COLUMN session_accept_until timestamp NULL;
`
