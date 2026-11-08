package model

import (
	"context"
	"fmt"

	"github.com/urnetwork/server"
)

// D-ACL-3's network_client.acl_subject_client_id (§2.8 api-4): the client whose
// grants a client acts under, written once at creation in the insert's own
// statement, with no added query. A top-level client is its own subject. A
// child takes its source client's subject, as sdk window clients act as their
// nearest subject ancestor, except a sidecar's own client, the child of its
// device's client that asks for strict_exp itself while that parent has none
// (owner decision 4, §7.4): it is its own subject, since the device client's
// would name the device. A sidecar's own window children inherit its strict_exp
// and so take its subject.
//
// The column belongs to MG2. A binary that does not link it leaves the column
// out of every insert, since the table has no such column then, and the
// clients it mints keep NULL, which reads as above until the backfill and the
// validated check of §19.7 settle every row.

// The column and value an INSERT INTO network_client adds for a top-level
// client, its own subject, clientIdParam being the insert's parameter for its
// client_id, such as "$1". Both empty while the binary does not link MG2.
func NetworkClientAclSubjectInsert(clientIdParam string) (column string, value string) {
	if !server.SidecarMg2Linked() {
		return "", ""
	}
	return ", acl_subject_client_id", ", " + clientIdParam
}

// The value a minted child's subject takes when it is not its own: its source
// client's subject, read from the source's row inside the insert. A source with
// no stored subject is either a top-level client minted before the column,
// which is its own subject, or such a child, whose subject the backfill
// resolves, so the new row keeps NULL and the same meaning.
const networkClientSourceAclSubjectSql = `(
	SELECT CASE
		WHEN acl_subject_parent.acl_subject_client_id IS NOT NULL THEN acl_subject_parent.acl_subject_client_id
		WHEN acl_subject_parent.source_client_id IS NULL THEN acl_subject_parent.client_id
	END
	FROM network_client acl_subject_parent
	WHERE acl_subject_parent.client_id = %s
)`

// Whether a client being minted is its own subject: a top-level client, or a
// sidecar's own client (above).
func newClientIsOwnAclSubject(sourceClientId *server.Id, requested clientMarks, parent clientMarks) bool {
	return sourceClientId == nil || (requested.strictExp && !parent.strictExp)
}

// The column and value AuthNetworkClient's insert adds, clientIdParam and
// sourceClientIdParam being its parameters for the client and its source. Both
// empty while the binary does not link MG2.
func networkClientMintAclSubjectInsert(ownSubject bool, clientIdParam string, sourceClientIdParam string) (column string, value string) {
	if !server.SidecarMg2Linked() {
		return "", ""
	}
	if ownSubject {
		return ", acl_subject_client_id", ", " + clientIdParam
	}
	return ", acl_subject_client_id", ", " + fmt.Sprintf(networkClientSourceAclSubjectSql, sourceClientIdParam)
}

// Fills the subjects of clients minted before the column, oldest client first,
// at most limit rows after afterClientId (the zero id from the start): a
// top-level client is its own; a sidecar's own client, which its binding in
// sidecar_identity_binding names, is its own, where NULL would have named its
// device's client; any other child takes its source's subject, or its source
// itself when the source is top-level or a sidecar's client. A child whose
// source has no subject yet stays NULL for a later pass. Returns the last
// client the batch read, how many it filled and how many it left, so an
// operator's command (§19.7's backfill, before the validated check) runs
// passes until one leaves none. It needs MG1's bindings and MG2's column.
func BackfillNetworkClientAclSubjects(ctx context.Context, afterClientId server.Id, limit int) (lastClientId server.Id, filledCount int64, leftCount int64, returnErr error) {
	if !server.SidecarMg1Linked() || !server.SidecarMg2Linked() {
		return afterClientId, 0, 0, ErrNetworkAclHeadUnavailable
	}
	if limit <= 0 {
		return afterClientId, 0, 0, fmt.Errorf("a backfill batch of %d rows", limit)
	}
	lastClientId = afterClientId
	server.Tx(ctx, func(tx server.PgTx) {
		filledCount, leftCount = 0, 0
		result, err := tx.Query(
			ctx,
			`
				WITH batch AS (
					SELECT
						child.client_id,
						CASE
							WHEN child.source_client_id IS NULL THEN child.client_id
							WHEN EXISTS (
								SELECT 1 FROM sidecar_identity_binding WHERE sidecar_identity_binding.client_id = child.client_id
							) THEN child.client_id
							WHEN parent.acl_subject_client_id IS NOT NULL THEN parent.acl_subject_client_id
							WHEN parent.client_id IS NOT NULL AND parent.source_client_id IS NULL THEN parent.client_id
							WHEN parent.client_id IS NOT NULL AND EXISTS (
								SELECT 1 FROM sidecar_identity_binding WHERE sidecar_identity_binding.client_id = parent.client_id
							) THEN parent.client_id
						END AS subject
					FROM network_client child
					LEFT JOIN network_client parent ON parent.client_id = child.source_client_id
					WHERE
						child.acl_subject_client_id IS NULL AND
						$1 < child.client_id
					ORDER BY child.client_id
					LIMIT $2
				),
				filled AS (
					UPDATE network_client
					SET acl_subject_client_id = batch.subject
					FROM batch
					WHERE
						network_client.client_id = batch.client_id AND
						network_client.acl_subject_client_id IS NULL AND
						batch.subject IS NOT NULL
					RETURNING network_client.client_id
				)
				SELECT
					(SELECT max(client_id::varchar)::uuid FROM batch),
					(SELECT count(*) FROM filled),
					(SELECT count(*) FROM batch WHERE subject IS NULL)
			`,
			afterClientId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var last *server.Id
				server.Raise(result.Scan(&last, &filledCount, &leftCount))
				if last != nil {
					lastClientId = *last
				}
			}
		})
	})
	return
}
