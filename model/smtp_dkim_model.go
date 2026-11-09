package model

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// Sending sidecars' DKIM public keys (§14.6, smtp-6; owner decision 36;
// SMTP-M9-18): smtp_dkim_key, MG9's. Each sidecar that sends signs under opaque
// selectors of its own, a network's d= or a replying burner's, and URnetwork
// holds only the public keys. POST /smtp/dkim-keys writes a row in the
// transaction that publishes its DNS record (SMTP-M9-19), a rotation retires a
// selector and keeps it published 7 days, and SMTP-M9-17 takes it out of DNS
// then and deletes the row 30 days after that, so a report that arrives late
// still checks against it. §14.8's report checks read rows through
// FindSmtpDkimKey, retired and removed ones included, and never DNS.
//
// Selectors and domains are stored lowercase and compared without case.
// Nothing here reads or writes the table unless the binary links MG9.

// A key's state.
const (
	// Current, and published in DNS.
	SmtpDkimKeyStatePublished = "published"
	// Rotated out: still in DNS for 7 days, then removed.
	SmtpDkimKeyStateRetired = "retired"
	// Out of DNS, 7 days after a rotation or at once on its sidecar's removal
	// (I-15); the row stays 30 days for late reports.
	SmtpDkimKeyStateRemoved = "removed"
)

// The algorithms a key may sign under (§14.6; RFC 6376, RFC 8463).
const (
	SmtpDkimAlgorithmRsaSha256     = "rsa-sha256"
	SmtpDkimAlgorithmEd25519Sha256 = "ed25519-sha256"
)

// Why key cannot be a smtp_dkim_key row, or nil.
func smtpDkimKeyRefusal(key *smtpedgeservice.SmtpDkimKey) error {
	refuse := func(field string) error {
		return fmt.Errorf("%w: a DKIM key's %s", ErrSmtpRowRefused, field)
	}
	switch {
	case key == nil:
		return refuse("row")
	case key.Selector == "" || !smtpTextStorable(key.Selector, smtpLabelMaxByteCount) || strings.Contains(key.Selector, "."):
		return refuse("selector")
	case key.Domain == "" || !smtpTextStorable(key.Domain, smtpDomainMaxByteCount):
		return refuse("domain")
	case key.NetworkId == (sidecar.Id{}):
		return refuse("network")
	case key.SidecarId == (sidecar.Id{}):
		return refuse("sidecar")
	case key.Algorithm != SmtpDkimAlgorithmRsaSha256 && key.Algorithm != SmtpDkimAlgorithmEd25519Sha256:
		return refuse("algorithm")
	case len(key.PublicKey) == 0:
		return refuse("public key")
	case key.Algorithm == SmtpDkimAlgorithmEd25519Sha256 && len(key.PublicKey) != ed25519.PublicKeySize:
		return refuse("Ed25519 public key")
	}
	return nil
}

// Publishes key in tx, state published as of publishTime: inserted false, and
// nothing changed, when its selector at its domain is stored already, since a
// selector names one key for good. The caller writes its DNS record in the same
// transaction (SMTP-M9-19).
func InsertSmtpDkimKeyInTx(ctx context.Context, tx server.PgTx, key *smtpedgeservice.SmtpDkimKey, publishTime time.Time) (inserted bool, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	if err := smtpDkimKeyRefusal(key); err != nil {
		returnErr = err
		return
	}
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO smtp_dkim_key (
			selector,
			domain,
			network_id,
			sidecar_id,
			algorithm,
			public_key,
			state,
			publish_time
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (selector, domain) DO NOTHING
		`,
		strings.ToLower(key.Selector),
		strings.ToLower(key.Domain),
		server.Id(key.NetworkId),
		server.Id(key.SidecarId),
		key.Algorithm,
		key.PublicKey,
		SmtpDkimKeyStatePublished,
		publishTime.UTC(),
	))
	inserted = tag.RowsAffected() == 1
	return
}

// InsertSmtpDkimKeyInTx in a transaction of its own, opened only for a row it
// can store.
func InsertSmtpDkimKey(ctx context.Context, key *smtpedgeservice.SmtpDkimKey, publishTime time.Time) (inserted bool, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	if returnErr = smtpDkimKeyRefusal(key); returnErr != nil {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		inserted, returnErr = InsertSmtpDkimKeyInTx(ctx, tx, key, publishTime)
	})
	return
}

// The key published under selector at domain, compared without case, whatever
// its state: a retired or removed key still checks reports until its row goes
// (§14.8). nil and no error when there is none.
func FindSmtpDkimKey(ctx context.Context, selector string, domain string) (key *smtpedgeservice.SmtpDkimKey, returnErr error) {
	if !server.SidecarMg9Linked() {
		returnErr = ErrSmtpEdgeUnlinked
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT selector, domain, network_id, sidecar_id, algorithm, public_key, retire_time
			FROM smtp_dkim_key
			WHERE selector = $1 AND domain = $2
			`,
			strings.ToLower(selector),
			strings.ToLower(domain),
		)
		server.WithPgResult(result, err, func() {
			if !result.Next() {
				return
			}
			var networkId server.Id
			var sidecarId server.Id
			var retireTime *time.Time
			key = &smtpedgeservice.SmtpDkimKey{}
			server.Raise(result.Scan(&key.Selector, &key.Domain, &networkId, &sidecarId, &key.Algorithm, &key.PublicKey, &retireTime))
			key.NetworkId = sidecar.Id(networkId)
			key.SidecarId = sidecar.Id(sidecarId)
			if retireTime != nil {
				key.RetireTime = *retireTime
			}
		})
	})
	return
}
