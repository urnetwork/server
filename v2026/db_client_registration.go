package server

// Registration bindings survive client revocation/deletion. Replaying an old
// request must never allocate a replacement identity. Network deletion retains
// its ordinary ownership and cleanup semantics.
const clientRegistrationSchemaSql = `
CREATE TABLE network_client_registration (
	network_id uuid NOT NULL REFERENCES network(network_id) ON DELETE CASCADE,
	registration_id varchar(64) NOT NULL CHECK (registration_id ~ '^[0-9a-f]{64}$'),
	request_sha256 varchar(64) NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
	scope_sha256 varchar(64) NOT NULL CHECK (scope_sha256 ~ '^[0-9a-f]{64}$'),
	authority_sha256 varchar(64) NOT NULL CHECK (authority_sha256 ~ '^[0-9a-f]{64}$'),
	user_id uuid NOT NULL,
	client_id uuid NOT NULL,
	device_id uuid NOT NULL,
	create_time timestamp NOT NULL,
	PRIMARY KEY (network_id, registration_id),
	UNIQUE (network_id, scope_sha256),
	UNIQUE (client_id)
);
`
