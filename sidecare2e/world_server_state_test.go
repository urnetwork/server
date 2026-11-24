//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Every read and write of server state the world makes outside the hosts and api's routes, in one file (the e2e
// conventions; QD-1028's layout move will move sidecar's model code): urnetwork-edge's network row, each block's Quota
// over api's internal quota surface, the quota ledgers a scenario reconciles, read after the debit journal is flushed
// as taskworker's flush would, the service debits by meter in service_debit_journal and network_service_usage, a
// network's verification by a paid balance, a device's issued certificate row standing in for the certs worker, the
// contract gate's install, api's dns.yml root, and a network's age and its mail enable's, which no route moves;
// tlsedge's connection records, which the blogger case's privacy capture reads; and a network's report token, which no
// zone of the world names in a rua (the World's header).

package sidecare2e

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/quotahost"
)

// Makes networkId network_verified (§21.7) as a network that bought data is, or one that subscribed to Pro when pro: a
// transfer balance of byteCount in window from now for duration, paid since it carries netRevenue
// (transfer_balance.paid's generated column) and carrying the Pro entitlement when pro (model.AddTransferBalance
// refreshing Pro's cache), then the verification's cache refreshed, as the payment's writer refreshes it
// (model.UpdateNetworkVerified). What §13.9's tls/ listen and §21.7's sending ask of a network, and what §8.5's
// standing publish asks of its document.
func worldVerifyNetwork(ctx context.Context, networkId server.Id, pro bool, byteCount int64, netRevenue int64, duration time.Duration) error {
	var returnErr error
	server.HandleError(func() {
		now := server.NowUtc()
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             now,
			EndTime:               now.Add(duration),
			StartBalanceByteCount: model.ByteCount(byteCount),
			BalanceByteCount:      model.ByteCount(byteCount),
			NetRevenue:            model.NanoCents(netRevenue),
			Pro:                   pro,
		})
		if pro && model.NetworkTierOf(ctx, networkId) != model.NetworkTierPro {
			returnErr = fmt.Errorf("network %s reads tier %s after its Pro balance", networkId, model.NetworkTierOf(ctx, networkId))
			return
		}
		if !model.UpdateNetworkVerified(ctx, networkId) {
			returnErr = fmt.Errorf("network %s reads unverified after its paid balance", networkId)
		}
	}, func(err error) {
		returnErr = fmt.Errorf("the paid balance: %w", err)
	})
	return returnErr
}

// The certificate row §13.8's certs worker writes once its CA issues device's certificate for the network's pair under
// root, <label>.<root> and *.<label>.<root> (cert-1's network_tls_cert, state issued), which §13.9's tls/ listen
// facts read: its CSR on the device's own TLS key over §13.8's identifier set, as the device's order carries it, and
// its validity from notBefore to notAfter. A stand-in for the certs worker and its ACME CA, which the world does not
// run (the World's header): no chain is issued, so a visitor is shown the device's own self-signed leaf on the key.
func worldIssueTlsCertificateRow(ctx context.Context, networkId server.Id, clientId server.Id, sidecarId server.Id, key *sidecar.TlsDeviceKey, root string, label string, notBefore time.Time, notAfter time.Time) error {
	names, err := sidecar.TlsIdentifierSet(root, label, "", nil)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: names}, key)
	if err != nil {
		return err
	}
	identifierSetSha256, err := sidecar.TlsIdentifierSetHash(names)
	if err != nil {
		return err
	}
	spkiSha256 := key.SpkiSha256()
	return model.NewCertsStore(nil).InsertCert(ctx, &sidecar.TlsCertRecord{
		CertId:              sidecar.NewId(),
		NetworkId:           sidecar.Id(networkId),
		ClientId:            sidecar.Id(clientId),
		SidecarId:           sidecar.Id(sidecarId),
		IdentifierSetSha256: identifierSetSha256[:],
		SpkiSha256:          spkiSha256[:],
		Attestation:         []byte{},
		Csr:                 csr,
		NotBefore:           notBefore,
		NotAfter:            notAfter,
		State:               sidecar.TlsCertStateIssued,
		StateTime:           notBefore,
	})
}

// What server holds for networkId's tls/ names, for a stalled visit's dump: the listen facts sockets reads
// (model.GetTlsListenFacts, api's internal listen-facts route) and the network's entries in root's label map, which
// tlsedge routes a server name by (model.DnsLabelMapSnapshot).
func worldTlsServerState(ctx context.Context, networkId server.Id, root string) string {
	lines := []string{}
	server.HandleError(func() {
		facts, err := model.GetTlsListenFacts(ctx, networkId, server.NowUtc())
		if err != nil {
			lines = append(lines, fmt.Sprintf("listen facts: %v", err))
		} else {
			encoded, _ := json.Marshal(facts)
			lines = append(lines, fmt.Sprintf("listen facts: %s", encoded))
		}
		labelMap, err := model.DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			lines = append(lines, fmt.Sprintf("label map of %s: %v", root, err))
			return
		}
		held := 0
		for _, entry := range labelMap.Entries {
			if entry.NetworkId != nil && *entry.NetworkId == networkId {
				held += 1
				lines = append(lines, fmt.Sprintf("label map of %s: %s %s %s at %d, suspended %q, withheld %v", root, entry.Label, entry.Kind, entry.State, entry.ChangeSerial, entry.SuspendReason, entry.WithheldHosts))
			}
		}
		if held == 0 {
			lines = append(lines, fmt.Sprintf("label map of %s, serial %d: no entry of the network among %d", root, labelMap.Serial, len(labelMap.Entries)))
		}
	}, func(err error) {
		lines = append(lines, fmt.Sprintf("the server state: %v", err))
	})
	return strings.Join(lines, "; ")
}

// tlsedge's connection records of networkId ending at or after since, as its store wrote them
// (model.Testing_ReadTlsConnectionRecords): what the world's privacy capture reads at each scan (tls-3's records). None
// where the read fails, which the capture's count of edge records shows.
func worldTlsConnectionRecords(ctx context.Context, networkId server.Id, since time.Time) (records []*tlsedgeservice.TlsConnectionRecord) {
	server.HandleError(func() {
		records = model.Testing_ReadTlsConnectionRecords(ctx, sidecar.Id(networkId), since)
	})
	return records
}

// networkId's label's change serial in root's label map, which tlsedge's reads since its last serial follow
// (model.DnsLabelMapSnapshot, DnsLabelMapSince): false while the network has no label there.
func worldDnsLabelChangeSerial(ctx context.Context, networkId server.Id, root string) (serial uint64, found bool, returnErr error) {
	server.HandleError(func() {
		labelMap, err := model.DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			returnErr = err
			return
		}
		for _, entry := range labelMap.Entries {
			if entry.NetworkId != nil && *entry.NetworkId == networkId && entry.Kind == model.DnsLabelKindLabel {
				serial, found = entry.ChangeSerial, true
				return
			}
		}
	}, func(err error) {
		returnErr = fmt.Errorf("the label map: %w", err)
	})
	return
}

// Serves root as api's dns.yml root (model.DnsUserRoot) until the returned pop: the root the world's dns API serves,
// as production's one dns.yml gives api and the dns block the same, so a label api draws for a network
// (GET /sidecar/label, GET /network/dns) is the one the dns API, tlsedge and mxedge read. The test stack's config
// carries no dns.yml, where api draws no label at all.
func worldSetDnsUserRoot(root string) func() {
	return model.Testing_SetDnsUserRoot(root)
}

// Puts limitKeyValues on pro.yml's pro: sidecar: block of the process's parsed pro.yml until the returned pop, beside
// the T3World's own tier (model.Testing_SetSidecarLimits): the world's second tier, as pro.yml carries each.
func worldInstallProLimits(limitKeyValues map[string]int64) func() {
	return model.Testing_SetSidecarLimits(model.NetworkTierPro, limitKeyValues)
}

// Whether the process's CreateContract consults §8.10's contract gate: sidecar.yml's acl_contract_gate switch on and
// a gate installed over api's gate cache (controller.ActiveAclContractGate, AclContractGateInstalled), as a world
// that turns the switch on has the T3World install it.
func worldContractGateActive() bool {
	return controller.AclContractGateInstalled() && controller.ActiveAclContractGate() != nil
}

// Moves networkId's network.create_time back by age, so §21.7's standing, which derives probation from it, reads the
// network age older: a network past ProbationAge (7 days) is standard, which internet mail asks (D-SMTP-14). The
// world's networks are made new, and no route moves a network's creation; what reads the standing caches it, so a
// world ages a network before anything reads it.
func worldAgeNetwork(ctx context.Context, networkId server.Id, age time.Duration) (returnErr error) {
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network SET create_time = create_time - $2 * INTERVAL '1 millisecond' WHERE network_id = $1`,
				networkId,
				age.Milliseconds(),
			))
			if tag.RowsAffected() != 1 {
				returnErr = fmt.Errorf("network %s has no row to age", networkId)
			}
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the network's age: %w", err)
	})
	return
}

// Moves networkId's mail_network.create_time, POST /mail/enable's time, back by age, so smtpedge's send gate, which
// starts internet mail MailEnableDelay (24 h) after it (§14.7), reads mail on that long. The gate's facts are cached
// for their refresh, so a world ages the enable before the network's first preamble.
func worldAgeMailEnable(ctx context.Context, networkId server.Id, age time.Duration) (returnErr error) {
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE mail_network SET create_time = create_time - $2 * INTERVAL '1 millisecond' WHERE network_id = $1`,
				networkId,
				age.Milliseconds(),
			))
			if tag.RowsAffected() != 1 {
				returnErr = fmt.Errorf("network %s has no mail_network row to age", networkId)
			}
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the mail enable's age: %w", err)
	})
	return
}

// networkId's report token in root's zone (network_dns_zone.report_token, MG6's; plan-SMTP-net-token), drawn with the
// zone's system set and kept for the network's life: the local part of the address its _dmarc and _smtp._tls rua name
// where dns.yml gives the root a report host, which the world's does not (the World's header).
func worldNetworkReportToken(ctx context.Context, networkId server.Id, root string) (token string, returnErr error) {
	server.HandleError(func() {
		var held *string
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT report_token FROM network_dns_zone WHERE root = $1 AND network_id = $2`, root, networkId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&held))
				}
			})
		})
		if held == nil {
			returnErr = fmt.Errorf("network %s's zone in %s holds no report token", networkId, root)
			return
		}
		token = *held
	}, func(err error) {
		returnErr = fmt.Errorf("the report token: %w", err)
	})
	return
}

// urnetwork-edge's network row, which api's edge client route mints the edges' clients in.
func worldCreateEdgeNetwork(ctx context.Context, networkId server.Id) {
	name := "e2eedge" + strings.ToLower(strings.ReplaceAll(networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, networkId, name, server.NewId())
}

// A block's Quota as its main builds one when it reads the ledgers through api (quotahost.NewBlockQuota): an api-backed
// HostQuota on caller's bearer at api's origin through T3Api, with the process's Redis for its rates and reservations.
// The returned end drains it, sending its open usage window, then ends it: a run calls it once its Run has returned.
func (self *World) blockQuota(caller server.InternalGateCaller) (*quotahost.HostQuota, func(), error) {
	transport, err := quotahost.NewHttpQuotaApi(self.apiOrigin(), self.bearer(caller), self.apiClient(), nil)
	if err != nil {
		return nil, nil, err
	}
	settings := quotahost.DefaultQuotaSettings()
	rateSettings := settings.Rate
	if rateSettings == nil {
		rateSettings = model.DefaultQuotaRateSettings()
	}
	ctx, cancel := context.WithCancel(context.Background())
	quota, err := quotahost.NewApiQuota(ctx, transport, model.NewQuotaRateServerRedis(rateSettings.CallTimeout), nil, settings)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return quota, func() {
		quota.Drain(context.Background())
		cancel()
	}, nil
}

// Applies every service debit the usage batches journaled to the balances, each shard's pages until none is left, as
// taskworker's flush does on its schedule: what a ledger read runs first, so a balance shows every debit applied.
func worldFlushServiceDebits(ctx context.Context) error {
	for shard := range model.ServiceDebitShardCount {
		var after *server.Id
		for {
			result, err := model.FlushServiceDebits(ctx, shard, after, sidecar.RealClock{})
			if err != nil {
				return fmt.Errorf("shard %d's debits: %w", shard, err)
			}
			if 0 < result.Failed {
				return fmt.Errorf("shard %d: %d balances' debits failed to apply", shard, result.Failed)
			}
			if !result.More {
				break
			}
			after = result.LastBalanceId
		}
	}
	return nil
}

// networkId's service debits in service_debit_journal the flush has not applied yet, by meter: each row is one usage
// batch's or carve's debit of one balance (§20.6), and its balance names the network. What the flush has applied is
// network_service_usage's (worldAppliedServiceUsage), an applied row's release still keeping it here a moment.
func worldJournaledServiceDebits(ctx context.Context, networkId server.Id) (meterByteCounts map[string]int64, returnErr error) {
	meterByteCounts = map[string]int64{}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT service_debit_journal.meter, SUM(service_debit_journal.debit_byte_count)
					FROM service_debit_journal
					INNER JOIN transfer_balance ON transfer_balance.balance_id = service_debit_journal.balance_id
					WHERE transfer_balance.network_id = $1 AND NOT service_debit_journal.applied
					GROUP BY service_debit_journal.meter
				`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var meter string
					var byteCount int64
					server.Raise(result.Scan(&meter, &byteCount))
					meterByteCounts[meter] += byteCount
				}
			})
		})
	}, func(err error) {
		returnErr = fmt.Errorf("service_debit_journal: %w", err)
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return meterByteCounts, nil
}

// networkId's applied service debits by meter: network_service_usage's quantity, bytes for every transfer meter,
// summed over its days, which the flush adds each applied journal row to on the day it applies it.
func worldAppliedServiceUsage(ctx context.Context, networkId server.Id) (meterByteCounts map[string]int64, returnErr error) {
	meterByteCounts = map[string]int64{}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT meter, SUM(quantity) FROM network_service_usage WHERE network_id = $1 GROUP BY meter`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var meter string
					var quantity int64
					server.Raise(result.Scan(&meter, &quantity))
					meterByteCounts[meter] += quantity
				}
			})
		})
	}, func(err error) {
		returnErr = fmt.Errorf("network_service_usage: %w", err)
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return meterByteCounts, nil
}

// networkId's quota ledger as server holds it: its in-window transfer balances, what each started with and what is
// left, and network_quota_state's stored bytes and spent mark.
func worldNetworkLedger(ctx context.Context, networkId server.Id) (*WorldNetworkLedger, error) {
	ledger := &WorldNetworkLedger{}
	for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
		ledger.StartBalanceByteCount += int64(balance.StartBalanceByteCount)
		ledger.BalanceByteCount += int64(balance.BalanceByteCount)
	}
	state, err := model.GetNetworkQuotaState(ctx, networkId)
	if err != nil {
		return nil, err
	}
	if state != nil {
		ledger.StoredByteCount = state.StoredByteCount
		ledger.Spent = state.SpentTime != nil
	}
	return ledger, nil
}
