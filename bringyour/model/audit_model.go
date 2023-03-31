package model

import (
	"context"
	"encoding/json"

	"bringyour.com/bringyour"
	"github.com/jackc/pgx/v5/pgxpool"
)

// compute stats (number of days)
//    uses the audit tables
// publish stats to redis (number of days)


// host: 192.168.208.135
// user: bringyour
// password: inning-volcano-prod-piddle
// db: bringyour


// SELECT now() - interval '7 days';

// provider daily stats


type Stats struct {
	// time
	// lookback

	AllTransferData map[string]int64 `json:"allTransferData"`
    AllTransferSummary int64 `json:"allTransferSummary"`
    AllTransferSummaryRate int64 `json:"allTransferSummaryRate"`

    AllPacketsData map[string]int64 `json:"allPacketsData"`
    AllPacketsSummary int64 `json:"allPacketsSummary"`
    AllPacketsSummaryRate int64 `json:"allPacketsSummaryRate"`

    ProvidersData map[string]int64 `json:"providersData"`
    ProvidersSummary int64 `json:"providersSummary"`
    ProvidersSummarySuperspeed int64 `json:"providersSummarySuperspeed"`

    CountriesData map[string]int64 `json:"countriesData"`
    CountriesSummary int64 `json:"countriesSummary"`

    RegionsData map[string]int64 `json:"regionsData"`
    RegionsSummary int64 `json:"regionsSummary"`

    CitiesData map[string]int64 `json:"citiesData"`
    CitiesSummary int64 `json:"citiesSummary"`

    ExtenderTransferData map[string]int64 `json:"extenderTransferData"`
    ExtenderTransferSummary int64 `json:"extenderTransferSummary"`
    ExtenderTransferSummaryRate int64 `json:"extenderTransferSummaryRate"`

    ExtendersData map[string]int64 `json:"extendersData"`
    ExtendersSummary int64 `json:"extendersSummary"`
    ExtendersSummarySuperspeed int64 `json:"extendersSummarySuperspeed"`

    NetworksData map[string]int64 `json:"networksData"`
    NetworksSummary int64 `json:"networksSummary"`

    DevicesData map[string]int64 `json:"devicesData"`
    DevicesSummary int64 `json:"devicesSummary"`
}



func ComputeStats90() Stats {
	// fixme
	bringyour.Db(func (context context.Context, conn *pgxpool.Conn) {

	})

	`
	SELECT t.day, t.device_id, audit_provider_event.event_id, audit_provider_event.event_type, audit_provider_event.country_name, audit_provider_event.region_name, audit_provider_event.city_name

	FROM (

	SELECT to_char(event_time, 'YYYY-MM-DD') AS day, device_id, MAX(event_id::varchar) AS max_event_id FROM audit_provider_event
	WHERE event_time <= now() AND now() - interval '90 days' <= event_time AND event_type IN ('provider_offline', 'provider_online_superspeed', 'provider_online_not_superspeed')
	GROUP BY day, device_id


	UNION ALL

	SELECT '0000-00-00' AS day, device_id, MAX(event_id::varchar) AS max_event_id FROM audit_provider_event
	WHERE event_time < now() - interval '90 days'
	GROUP BY device_id

	) t

	INNER JOIN audit_provider_event ON t.max_event_id::uuid = audit_provider_event.event_id
	;

	`

	// extender daily stats
	`
	SELECT t.day, t.extender_id, audit_extender_event.event_id, audit_extender_event.event_type

	FROM (

	SELECT to_char(event_time, 'YYYY-MM-DD') AS day, extender_id, MAX(event_id::varchar) AS max_event_id FROM audit_extender_event
	WHERE event_time <= now() AND now() - interval '90 days' <= event_time AND event_type IN ('extender_offline', 'extender_online_superspeed', 'extender_online_not_superspeed')
	GROUP BY day, extender_id


	UNION ALL

	SELECT '0000-00-00' AS day, extender_id, MAX(event_id::varchar) AS max_event_id FROM audit_extender_event
	WHERE event_time < now() - interval '90 days'
	GROUP BY extender_id

	) t

	INNER JOIN audit_extender_event ON t.max_event_id::uuid = audit_extender_event.event_id
	;
	`


	// network daily stats
	`
	SELECT t.day, t.network_id, audit_network_event.event_id, audit_network_event.event_type

	FROM (

	SELECT to_char(event_time, 'YYYY-MM-DD') AS day, network_id, MAX(event_id::varchar) AS max_event_id FROM audit_network_event
	WHERE event_time <= now() AND now() - interval '90 days' <= event_time AND event_type IN ('network_created', 'network_deleted')
	GROUP BY day, network_id


	UNION ALL

	SELECT '0000-00-00' AS day, network_id, MAX(event_id::varchar) AS max_event_id FROM audit_network_event
	WHERE event_time < now() - interval '90 days'
	GROUP BY network_id

	) t

	INNER JOIN audit_network_event ON t.max_event_id::uuid = audit_network_event.event_id
	;
	`


	// device daily stats

	`
	SELECT t.day, t.device_id, audit_device_event.event_id, audit_device_event.event_type

	FROM (

	SELECT to_char(event_time, 'YYYY-MM-DD') AS day, device_id, MAX(event_id::varchar) AS max_event_id FROM audit_device_event
	WHERE event_time <= now() AND now() - interval '90 days' <= event_time AND event_type IN ('device_added', 'device_removed')
	GROUP BY day, device_id


	UNION ALL

	SELECT '0000-00-00' AS day, device_id, MAX(event_id::varchar) AS max_event_id FROM audit_device_event
	WHERE event_time < now() - interval '90 days'
	GROUP BY device_id

	) t

	INNER JOIN audit_device_event ON t.max_event_id::uuid = audit_device_event.event_id
	;
	`


	// all transfer

	`

	SELECT to_char(event_time, 'YYYY-MM-DD') AS day, SUM(transfer_bytes) AS net_transfer_bytes FROM audit_contract_event
	WHERE event_time <= now() AND now() - interval '90 days' <= event_time AND event_type IN ('contract_closed_success')
	GROUP BY day


	UNION ALL

	SELECT '0000-00-00' AS day, SUM(transfer_bytes) AS net_transfer_bytes FROM audit_contract_event
	WHERE event_time < now() - interval '90 days' AND event_type IN ('contract_closed_success')

	;
	`


	// all packets

	`

	SELECT to_char(event_time, 'YYYY-MM-DD') AS day, SUM(transfer_packets) AS net_transfer_packets FROM audit_contract_event
	WHERE event_time <= now() AND now() - interval '90 days' <= event_time AND event_type IN ('contract_closed_success')
	GROUP BY day


	UNION ALL

	SELECT '0000-00-00' AS day, SUM(transfer_packets) AS net_transfer_packets FROM audit_contract_event
	WHERE event_time < now() - interval '90 days' AND event_type IN ('contract_closed_success')

	;
	`



	// extender transfer

	`

	SELECT to_char(event_time, 'YYYY-MM-DD') AS day, SUM(transfer_bytes) AS net_transfer_bytes FROM audit_contract_event
	WHERE event_time <= now() AND now() - interval '90 days' <= event_time AND extender_id IS NOT NULL AND event_type IN ('contract_closed_success')
	GROUP BY day


	UNION ALL

	SELECT '0000-00-00' AS day, SUM(transfer_bytes) AS net_transfer_bytes FROM audit_contract_event
	WHERE event_time < now() - interval '90 days' AND event_type IN ('contract_closed_success')

	;
	`

}


func ExportStats(stats Stats) {
	// set into redis
	json := json.Marshal(stats)

}

func GetExportedStatsJson(lookback int) string {
	Redis(func(context Context, client Client) {

	})
	// read from redis
}

func GetExportedStats(lookback int) Stats {
	statsJson := GetExportedStatsJson()
	// convert to stats
}







type AuditProviderCheck struct {

}
func newAuditProviderCheck() *AuditProviderCheck {

}


type AuditExtenderCheck struct {

}
func newAuditExtenderCheck() {

}


type AuditNetworkEvent struct {

}
func newAuditNetworkEvent() {

}


type AuditDeviceEvent struct {

}
func newAuditDeviceEvent() {

}


type AuditContractEvent struct {

}
func newAuditContractEvent() {

}


func AddEvent(event interface{}) {
	switch v := event.(type) {
	case AuditProviderCheck:
		Db(func(context Context, conn Conn) {
			conn.insert(`
				INSERT INTO Foo
			`)
		})
	case AuditExtenderCheck:
	case AuditNetworkEvent:
	case AuditDeviceEvent:
	case AuditContractEvent:
	default:
		panic("Event type not recognized: %T", v)
	}
}







