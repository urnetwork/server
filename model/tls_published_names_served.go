package model

// The names a network serves its published hosts under (QD-770; §12.5, §12.8,
// §13.5): what taskworker's published-names source joins each grant of the
// network's verified state (sidecar.AclState.PublishedNames) to, so §21.6's
// check reads a grant under every name a visitor reaches it by.
//
//	labels        each live label and live alias of the network in dns_label,
//	              in every root (MG2): tlsedge routes <host>.<x>.<root>, and
//	              <x>.<root> as @, for x a live label or alias (§13.5). A
//	              retiring or tombstoned name routes nothing and is not read
//	custom hosts  each active custom host of the network
//	              (network_dns_custom_host, MG5) that is one of its verified
//	              domains (network_domain, MG6) or ends in one at a label
//	              boundary, joined to the grant whose host is the custom host's
//	              owner label (DESIGN-DETAILS 17.2.2). A binary that does not
//	              link both groups holds no custom host. A glob host joins no
//	              custom host: its owner is one label, which this does not match
//	              against a glob
//
// A grant the network serves under none of these is listed once with no name,
// so the check still sees that the network publishes it.

import (
	"context"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// network_dns_custom_host.state of a host the custom-host map routes (§12.8).
const tlsCustomHostStateActive = "active"

// One live label or alias of a network: the name x of <host>.<x>.<root>.
type tlsServedLabel struct {
	// lowercase with no trailing dot
	root  string
	label string
}

// One active custom host of a network under a verified domain, and the owner
// label it maps to.
type tlsServedCustomHost struct {
	host  string
	owner string
}

// The network's live labels and aliases in every root, by root and label.
func getTlsServedLabels(ctx context.Context, networkId server.Id) []*tlsServedLabel {
	labels := []*tlsServedLabel{}
	if !server.SidecarMg2Linked() {
		return labels
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT root, label
				FROM dns_label
				WHERE network_id = $1 AND state = $2 AND kind IN ($3, $4)
				ORDER BY root, label
			`,
			networkId,
			DnsLabelStateLive,
			DnsLabelKindLabel,
			DnsLabelKindAlias,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				label := &tlsServedLabel{}
				server.Raise(result.Scan(&label.root, &label.label))
				label.root = strings.TrimSuffix(label.root, ".")
				labels = append(labels, label)
			}
		})
	})
	return labels
}

// The network's active custom hosts under its verified domains, by host.
func getTlsServedCustomHosts(ctx context.Context, networkId server.Id) []*tlsServedCustomHost {
	customHosts := []*tlsServedCustomHost{}
	if !server.SidecarMg5Linked() || !server.SidecarMg6Linked() {
		return customHosts
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT custom_host.host, custom_host.owner
				FROM network_dns_custom_host custom_host
				WHERE custom_host.network_id = $1 AND custom_host.state = $2 AND EXISTS (
					SELECT 1
					FROM network_domain verified
					WHERE
						verified.network_id = custom_host.network_id AND
						verified.verify_time IS NOT NULL AND (
							rtrim(lower(custom_host.host), '.') = verified.domain OR
							right(rtrim(lower(custom_host.host), '.'), length(verified.domain) + 1) = '.' || verified.domain
						)
				)
				ORDER BY custom_host.host
			`,
			networkId,
			tlsCustomHostStateActive,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				customHost := &tlsServedCustomHost{}
				server.Raise(result.Scan(&customHost.host, &customHost.owner))
				customHost.host = strings.TrimSuffix(customHost.host, ".")
				customHosts = append(customHosts, customHost)
			}
		})
	})
	return customHosts
}

// The list's names of a network's published grants (the file header): each
// grant under each live label and alias of the network, and under each of its
// custom hosts whose owner is the grant's host, or once with no name where it
// has none of these. Reads nothing for a network that publishes nothing.
func TlsPublishedNamesOf(ctx context.Context, networkId server.Id, grants []*sidecar.AclPublishedName) []*TlsPublishedName {
	names := []*TlsPublishedName{}
	if len(grants) == 0 {
		return names
	}
	labels := getTlsServedLabels(ctx, networkId)
	customHosts := getTlsServedCustomHosts(ctx, networkId)
	for _, grant := range grants {
		if grant == nil {
			continue
		}
		listed := func(label string, alias string, servedName string) {
			name := &TlsPublishedName{
				NetworkId: networkId,
				Host:      grant.Host,
				Port:      grant.Port,
				Label:     label,
				Alias:     alias,
				Name:      servedName,
				Audience:  grant.Audience,
			}
			if grant.ExpiresMs != 0 {
				name.ExpireTime = time.UnixMilli(grant.ExpiresMs).UTC()
			}
			names = append(names, name)
		}
		served := false
		for _, label := range labels {
			servedName := label.label + "." + label.root
			if grant.Host != "@" {
				servedName = grant.Host + "." + servedName
			}
			listed(label.label, "", servedName)
			served = true
		}
		for _, customHost := range customHosts {
			if customHost.owner == grant.Host {
				listed("", customHost.host, customHost.host)
				served = true
			}
		}
		if !served {
			listed("", "", "")
		}
	}
	return names
}
