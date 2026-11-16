package model

// TLS-M5-22's listen facts (§13.9; plan-SOCK-gate-data's GET /internal/gate/listen-facts/<network_id>; QD-107,
// QD-188, QD-807): what tls/'s ValidateListen reads of a network beyond its signed projection, as
// socketsservice.TlsListenFacts. Each is server state a gate keeps in memory and reads off the registration path:
//
//   - network_verified and §21.7's standing, from one read of network and network_abuse_state
//     (loadNetworkAbuseFactsWithConn), as ABUSE's own gate reads them;
//   - §20.5's tier (NetworkTierOf), and the end of §20.7's 14 days from a fall from Pro or Team
//     (NetworkDowngradeTime), through which a Community network keeps its standing grants;
//   - whether a mail_network row exists (§15.15), MG7's;
//   - the network's issued network_tls_cert rows not yet past not_after, MG5's, each by what it names. A row's names
//     are its CSR's, which certs held to the row's identifier set when it took the order, or for a row no request
//     preceded its leaf's, read against the network's labels so its own pair and its scopes' pairs are told apart;
//   - the sidecars bound to a client with a proxy_device_config row, or to that client's child: §18's hosted
//     devices, which D-TLS-1 bars from tls/;
//   - ScreenHost's facts: the verified domains, the Team scope labels (none known reads as none, as dns's writes
//     read them) and the hosts a reviewer allowed, which stay empty until ABUSE-M5-08's screen_allow exists.
//
// A network that does not exist is an error, never empty facts, so the hooks keep it undecided. A table the binary
// does not link holds no row here: no certificate, no mail_network row, no binding. Nothing here is on a
// connection's path (I-12). A process that holds Postgres hands GetTlsListenFacts to the hooks as their source; the
// sockets block holds none and reads these through api's internal route, which plan-SOCK-gate-data names and which
// is not built yet.

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/socketsservice"

	"github.com/urnetwork/server"
)

// The network does not exist, so nothing about it may pass.
var ErrTlsListenFactsNetworkNotFound = errors.New("tls listen facts: no such network")

// The network holds more live certificate rows than one read takes.
var ErrTlsListenFactsTooManyCerts = errors.New("tls listen facts: too many certificate rows")

// Tunables of the read.
type TlsListenFactsSettings struct {
	// §20.7's 14 days from a downgrade, through which a network keeps what its old tier allowed: here, its standing
	// publish grants (QD-107). QUOTA owns the clock (QUOTA-M6-05's ApplyNetworkDowngrades), and both read the same 14
	// days.
	DowngradeGrace time.Duration
	// The most live certificate rows one read takes. A network past it is an error, which keeps its tls/ entries
	// undecided rather than judging them on part of its rows.
	CertMaxCount int
}

// §20.7's 14 days, and room for every device and scope of a large Team twice over.
func DefaultTlsListenFactsSettings() *TlsListenFactsSettings {
	return &TlsListenFactsSettings{
		DowngradeGrace: 14 * 24 * time.Hour,
		CertMaxCount:   4096,
	}
}

// The network's listen facts at now, read from Postgres with the defaults. Not for an inline path.
func GetTlsListenFacts(ctx context.Context, networkId server.Id, now time.Time) (*socketsservice.TlsListenFacts, error) {
	return GetTlsListenFactsWithSettings(ctx, networkId, now, DefaultTlsListenFactsSettings())
}

// GetTlsListenFacts as a socketsservice.TlsListenFactsSource on now's clock, for a process that holds Postgres.
func TlsListenFactsSource(now func() time.Time) socketsservice.TlsListenFactsSource {
	if now == nil {
		now = server.NowUtc
	}
	return socketsservice.TlsListenFactsFunc(func(ctx context.Context, networkId connect.Id) (*socketsservice.TlsListenFacts, error) {
		return GetTlsListenFacts(ctx, server.Id(networkId), now())
	})
}

// GetTlsListenFacts with settings.
func GetTlsListenFactsWithSettings(ctx context.Context, networkId server.Id, now time.Time, settings *TlsListenFactsSettings) (facts *socketsservice.TlsListenFacts, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("tls listen facts: %v", r)
			}
			facts = nil
		}
	}()
	facts = &socketsservice.TlsListenFacts{}
	// each read below takes its own connection, one at a time (CODESTYLE's locking)
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		abuseFacts, ok := loadNetworkAbuseFactsWithConn(ctx, conn, []server.Id{networkId}, now, true)[networkId]
		if !ok {
			return
		}
		found = true
		standing := deriveNetworkAbuseStanding(networkId, abuseFacts.createTime, abuseFacts.row, now, DefaultAbuseSettings())
		facts.Verified = abuseFacts.verified
		facts.Standing = sidecar.AbuseStanding(standing.Standing)
		facts.MailNetwork = tlsListenMailNetworkWithConn(ctx, conn, networkId)
		facts.HostedSidecarIds = tlsListenHostedSidecarIdsWithConn(ctx, conn, networkId)
	})
	if !found {
		return nil, ErrTlsListenFactsNetworkNotFound
	}
	facts.Tier = string(NetworkTierOf(ctx, networkId))
	if downgradeTime, _, ok := NetworkDowngradeTime(ctx, networkId); ok {
		if endTime := downgradeTime.Add(settings.DowngradeGrace); now.Before(endTime) {
			facts.StandingGrantsEndTime = endTime
		}
	}
	certs, err := tlsListenCerts(ctx, networkId, now, settings)
	if err != nil {
		return nil, err
	}
	facts.Certs = certs
	facts.VerifiedDomains = GetNetworkVerifiedDomains(ctx, networkId)
	if scopeLabels, ok := dnsNetworkScopeLabels(ctx, networkId); ok {
		facts.ScopeLabels = scopeLabels
	}
	// 17.2.10's two lists, from TLS-M6-05's rows: with none, every listed name is dropped, the closed direction
	if dnsCustomHostLinked() {
		server.Db(ctx, func(conn server.PgConn) {
			facts.CustomCertified, facts.AcmeClaims = getTlsCustomListenFactsInTx(ctx, conn, networkId, now)
		})
	}
	return facts, nil
}

// Whether the network has a mail_network row, false in a binary that does not link MG7.
func tlsListenMailNetworkWithConn(ctx context.Context, query server.PgCanQuery, networkId server.Id) bool {
	if !server.SidecarMg7Linked() {
		return false
	}
	exists := false
	result, err := query.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM mail_network WHERE network_id = $1)`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&exists))
		}
	})
	return exists
}

// The network's sidecars bound, in any generation, to a hosted device or a hosted device's child (§18; D-SDK-12):
// a client with a proxy_device_config row, or one whose source client has one. Read from the network's identities,
// so it costs what the network's sidecars cost and not what its clients do. None in a binary without MG1.
func tlsListenHostedSidecarIdsWithConn(ctx context.Context, query server.PgCanQuery, networkId server.Id) []connect.Id {
	sidecarIds := []connect.Id{}
	if !server.SidecarMg1Linked() {
		return sidecarIds
	}
	result, err := query.Query(
		ctx,
		`
			SELECT DISTINCT sidecar_identity.sidecar_id
			FROM sidecar_identity
			INNER JOIN sidecar_identity_binding ON
				sidecar_identity_binding.sidecar_id = sidecar_identity.sidecar_id
			INNER JOIN network_client ON
				network_client.client_id = sidecar_identity_binding.client_id
			WHERE
				sidecar_identity.network_id = $1 AND (
					EXISTS (
						SELECT 1 FROM proxy_device_config
						WHERE proxy_device_config.client_id = network_client.client_id
					) OR
					EXISTS (
						SELECT 1 FROM proxy_device_config
						WHERE proxy_device_config.client_id = network_client.source_client_id
					)
				)
			ORDER BY sidecar_identity.sidecar_id
		`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var sidecarId server.Id
			server.Raise(result.Scan(&sidecarId))
			sidecarIds = append(sidecarIds, connect.Id(sidecarId))
		}
	})
	return sidecarIds
}

// One live row as the read takes it.
type tlsListenCertRow struct {
	sidecarId server.Id
	notAfter  time.Time
	csr       []byte
	leaf      []byte
}

// The network's issued rows not past not_after at now, each by what it names against the network's labels. None in
// a binary without MG5.
func tlsListenCerts(ctx context.Context, networkId server.Id, now time.Time, settings *TlsListenFactsSettings) ([]*socketsservice.TlsListenCert, error) {
	certs := []*socketsservice.TlsListenCert{}
	if !server.SidecarMg5Linked() {
		return certs, nil
	}
	rows := []*tlsListenCertRow{}
	zones := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT sidecar_id, not_after, csr, chain[1]
				FROM network_tls_cert
				WHERE
					network_id = $1 AND
					state = $2 AND
					not_after IS NOT NULL AND
					$3 < not_after
				ORDER BY cert_id DESC
				LIMIT $4
			`,
			networkId,
			sidecar.TlsCertStateIssued,
			now.UTC(),
			settings.CertMaxCount+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &tlsListenCertRow{}
				server.Raise(result.Scan(&row.sidecarId, &row.notAfter, &row.csr, &row.leaf))
				rows = append(rows, row)
			}
		})
		if len(rows) == 0 || !server.SidecarMg2Linked() {
			return
		}
		// the network's own zone in each root it holds a label in: <label>.<root>, which its pair names
		for _, root := range getNetworkDnsRootsInTx(ctx, conn, networkId) {
			if label := getNetworkLabelInTx(ctx, conn, networkId, root); label != "" {
				zones = append(zones, label+"."+strings.TrimSuffix(root, "."))
			}
		}
	})
	if settings.CertMaxCount < len(rows) {
		return nil, ErrTlsListenFactsTooManyCerts
	}
	for _, row := range rows {
		certs = append(certs, tlsListenCertOf(row, zones))
	}
	return certs, nil
}

// What a row names: its CSR's names, else its leaf's; a row whose names cannot be read names nothing, which no host
// rides.
func tlsListenCertNames(row *tlsListenCertRow) []string {
	if 0 < len(row.csr) {
		if request, err := x509.ParseCertificateRequest(row.csr); err == nil {
			return request.DNSNames
		}
	}
	if 0 < len(row.leaf) {
		if leaf, err := x509.ParseCertificate(row.leaf); err == nil {
			return leaf.DNSNames
		}
	}
	return nil
}

// The row as ValidateListen reads it: the network's pair where it names {*.<zone>, <zone>} for one of zones, and each
// scope s whose {*.<s>.<zone>, <s>.<zone>} it names.
func tlsListenCertOf(row *tlsListenCertRow, zones []string) *socketsservice.TlsListenCert {
	cert := &socketsservice.TlsListenCert{
		SidecarId: connect.Id(row.sidecarId),
		NotAfter:  row.notAfter.UTC(),
	}
	names := map[string]bool{}
	for _, name := range tlsListenCertNames(row) {
		names[strings.ToLower(strings.TrimSuffix(name, "."))] = true
	}
	scopes := []string{}
	for _, zone := range zones {
		if names[zone] && names["*."+zone] {
			cert.NetworkPair = true
		}
		for name := range names {
			scope, found := strings.CutSuffix(name, "."+zone)
			if found && scope != "" && !strings.Contains(scope, ".") && !strings.HasPrefix(scope, "*") && names["*."+name] {
				scopes = append(scopes, scope)
			}
		}
	}
	slices.Sort(scopes)
	if scopes = slices.Compact(scopes); 0 < len(scopes) {
		cert.ScopePairs = scopes
	}
	return cert
}
