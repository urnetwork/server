package model

import (
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
