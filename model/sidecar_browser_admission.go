package model

import (
	"context"

	"github.com/urnetwork/server"
)

// The requested browser's current binding only when both active bound clients are devices of the
// caller's live member. Generic identity reads and their cross-network share rule are independent.
func GetOwnBrowserAdmissionIdentity(ctx context.Context, member *NetworkSignIn, callerClientId, targetId server.Id, ephemeral bool) *SidecarIdentityView {
	memberId := member.UserId
	personal := member.MemberId == nil
	if !personal {
		memberId = *member.MemberId
	}
	var view *SidecarIdentityView
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `
			SELECT target.identity_public_key, target.identity_alg, target.status, binding.signatures
			FROM sidecar_identity target
			JOIN LATERAL (
				SELECT client_id, signatures FROM sidecar_identity_binding
				WHERE sidecar_id = target.sidecar_id ORDER BY generation DESC LIMIT 1
			) binding ON true
			JOIN network_client candidate ON candidate.client_id = binding.client_id AND candidate.network_id = target.network_id
			JOIN network_client_owner holder ON holder.client_id = candidate.client_id
			WHERE target.network_id = $1 AND target.sidecar_id = $2 AND target.status = 'active'
				AND candidate.active AND candidate.source_client_id IS NULL AND candidate.client_id <> $3
				AND holder.owner_user_id = $4 AND (holder.member_id = $5 OR ($6 AND holder.member_id IS NULL))
				AND NOT holder.tagged AND holder.auth_key_id IS NULL AND holder.admin_refused AND holder.strict_exp
				AND holder.declares_no_egress AND holder.ephemeral = $7
				AND EXISTS (
					SELECT 1 FROM network_client caller
					JOIN network_client_owner signer ON signer.client_id = caller.client_id
					JOIN sidecar_identity_binding caller_binding ON caller_binding.client_id = caller.client_id
					JOIN sidecar_identity identity ON identity.sidecar_id = caller_binding.sidecar_id AND identity.network_id = caller.network_id
					WHERE caller.client_id = $3 AND caller.network_id = target.network_id AND caller.active AND identity.status = 'active'
						AND signer.owner_user_id = $4 AND (signer.member_id = $5 OR ($6 AND signer.member_id IS NULL)) AND NOT signer.tagged AND NOT signer.ephemeral
						AND NOT EXISTS (SELECT 1 FROM sidecar_identity_binding later WHERE later.sidecar_id = caller_binding.sidecar_id AND later.generation > caller_binding.generation)
				)`, member.NetworkId, targetId, callerClientId, member.UserId, memberId, personal, ephemeral)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				view = &SidecarIdentityView{SidecarId: targetId, NetworkId: member.NetworkId}
				server.Raise(rows.Scan(&view.IdentityPublicKey, &view.IdentityAlg, &view.Status, &view.Binding))
				view.Bindings = [][]byte{view.Binding}
			}
		})
	})
	return view
}
