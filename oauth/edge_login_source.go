package oauth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/sidecar"
)

// An origin under a served root belongs only to the network holding its live label or
// alias. Its current verified ACL grants the named HTTP endpoint to members.
type projectionEdgeLoginSource struct {
	gate ExchangeGate
}

func NewProjectionEdgeLoginSource(gate ExchangeGate) EdgeLoginSource {
	return &projectionEdgeLoginSource{gate: gate}
}

func edgeLoginRoot(ctx context.Context, host string) (root string, err error) {
	if !server.SidecarMg5Linked() {
		return "", nil
	}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT root FROM dns_root
                WHERE $1 = root OR right($1, length(root)+1) = '.' || root
                ORDER BY length(root) DESC LIMIT 1`, host)
			server.WithPgResult(rows, err, func() {
				if rows.Next() {
					server.Raise(rows.Scan(&root))
				}
				server.Raise(rows.Err())
			})
		})
	}, func(cause error) { err = cause })
	return
}

func (self *projectionEdgeLoginSource) ResolveOrigin(ctx context.Context, originHost string) (server.Id, string, bool, error) {
	root, err := edgeLoginRoot(ctx, originHost)
	if err != nil || root == "" {
		return server.Id{}, "", false, err
	}
	labels, under := strings.CutSuffix(originHost, "."+root)
	if !under || labels == "" {
		return server.Id{}, "", true, ErrEdgeOriginRefused
	}
	host, label := "@", labels
	if i := strings.LastIndexByte(labels, '.'); i >= 0 {
		host, label = labels[:i], labels[i+1:]
	}
	networkId, kind, err := model.ResolveNetworkLabel(ctx, root, label, false)
	if errors.Is(err, sidecar.ErrNetworkLabelUnknown) || kind == sidecar.NetworkLabelKindRetiringAlias {
		return server.Id{}, "", true, ErrEdgeOriginRefused
	}
	if err != nil {
		return server.Id{}, "", true, err
	}
	return networkId, host, true, nil
}

func (self *projectionEdgeLoginSource) MembersPublishGranted(ctx context.Context, networkId server.Id, host string, port int, now time.Time) (bool, error) {
	snapshot, err := verifiedGateSnapshot(self.gate, networkId)
	if err != nil {
		return false, err
	}
	return AclMembersPublishGranted(snapshot.Acl, host, port, now), nil
}
