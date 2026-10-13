package model

import (
	"context"
	"errors"

	"github.com/urnetwork/server/v2026"
)

// ClientContractMetadata is the destination's optional, published handshake
// material. A missing or invalid public key does not discard a valid TLS chain.
type ClientContractMetadata struct {
	TLSCertificatePEM             []byte
	ClientKeySignedTLSCertificate []byte
	PublicKey                     []byte
}

// GetClientContractMetadata replaces the two simultaneous database acquisitions
// made by every CreateContract with one indexed read. The signed-key authority
// is identical to GetClientPublicKey: retired/tombstoned history suppresses the
// legacy key, and verification errors never fall through to Redis. All external
// work and signature verification happens after releasing PostgreSQL.
func GetClientContractMetadata(ctx context.Context, clientID server.Id) (metadata ClientContractMetadata, resultErr error) {
	if ctx == nil {
		return metadata, errors.New("client contract metadata identity is invalid")
	}
	if err := ctx.Err(); err != nil {
		return metadata, err
	}
	var projection stClientKeyCurrentProjection
	var queryErr error
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
			SELECT c.tls_certificate_pem, c.client_key_signed_tls_certificate,
				r.client_id IS NOT NULL, COALESCE(h.retired, false),
				EXISTS (SELECT 1 FROM network_client n WHERE n.client_id = h.client_id AND n.network_id = h.network_id AND n.active = true),
				COALESCE(h.network_id, '00000000-0000-0000-0000-000000000000'::uuid),
				COALESCE(h.generation, 0), h.domain_hash,
				r.registration, r.registration_hash, r.evidence, COALESCE(r.evidence_hash, '')
			FROM (SELECT $1::uuid AS client_id) requested
			LEFT JOIN client_tls_certificate c ON c.client_id = requested.client_id
			LEFT JOIN (st_client_key_head h JOIN st_client_key_history r
				ON r.client_id = h.client_id AND r.domain_hash = h.domain_hash AND r.generation = h.generation)
				ON h.client_id = requested.client_id AND h.is_current
		`, clientID).Scan(&metadata.TLSCertificatePEM, &metadata.ClientKeySignedTLSCertificate,
				&projection.found, &projection.retired, &projection.clientExists, &projection.networkID,
				&projection.generation, &projection.domainHash, &projection.registrationBytes,
				&projection.registrationHash, &projection.evidenceBytes, &projection.evidenceHash))
		})
	}, func(err error) { queryErr = err })
	if queryErr != nil {
		// The former independent reads could retain the certificate when
		// signed-key SQL failed. Retry only that optional certificate once,
		// after releasing the failed acquisition and only for a live caller.
		// A failed signed-authority read must never permit a Redis key fallback.
		metadata = ClientContractMetadata{}
		if ctx.Err() == nil {
			server.HandleError(func() {
				metadata.TLSCertificatePEM, metadata.ClientKeySignedTLSCertificate, resultErr = GetClientTlsCertificateAndSignature(ctx, clientID)
			}, func(err error) { resultErr = err })
		}
		return metadata, errors.Join(queryErr, resultErr, ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return metadata, err
	}
	// ControlId is zero and can have a published certificate. The old TLS
	// reader allowed that identity while GetClientPublicKey rejected it.
	if clientID == (server.Id{}) {
		return metadata, errors.New("client-key current lookup identity is invalid")
	}
	if projection.found {
		metadata.PublicKey, resultErr = projection.publicKey(clientID)
	} else {
		// Redis connection setup can raise as well as return an error. Keep
		// its optional failure separate from the already-read certificate,
		// matching the old independent metadata readers.
		server.HandleError(func() {
			metadata.PublicKey, resultErr = getLegacyClientPublicKey(ctx, clientID)
		}, func(err error) {
			metadata.PublicKey = nil
			resultErr = err
		})
	}
	return metadata, errors.Join(resultErr, ctx.Err())
}
