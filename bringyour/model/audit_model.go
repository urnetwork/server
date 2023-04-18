package model

import (
	"context"
	"fmt"
	"encoding/json"
	"strings"

	"bringyour.com/bringyour"
)

type AuditEventType string
const (
	AuditEventTypeProviderOffline AuditEventType = "provider_offline"
	AuditEventTypeProviderOnlineSuperspeed AuditEventType = "provider_online_superspeed"
	AuditEventTypeProviderOnlineNotSuperspeed AuditEventType = "provider_online_not_superspeed"
	AuditEventTypeExtenderOffline AuditEventType = "extender_offline"
	AuditEventTypeExtenderOnlineSuperspeed AuditEventType = "extender_online_superspeed"
	AuditEventTypeExtenderOnlineNotSuperspeed AuditEventType = "extender_online_not_superspeed"
	AuditEventTypeNetworkCreated AuditEventType = "network_created"
	AuditEventTypeNetworkDeleted AuditEventType = "network_deleted"
	AuditEventTypeDeviceAdded AuditEventType = "device_added"
	AuditEventTypeDeviceRemoved AuditEventType = "device_removed"
	AuditEventTypeContractClosedSuccess AuditEventType = "contract_closed_success"
)


type Stats struct {
	// fixme
	// time
	// lookback
	Lookback int
	AsOf int64

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



func ComputeStats90() *Stats {
	lookback := 90

	stats := &Stats{
		Lookback: lookback,
		AsOf: Now(),
	}

	bringyour.Db(func (context context.Context, conn bringyour.PgConn) {
		// provider daily stats + cities, regions, countries
		computerStatsProvider(stats, context, conn)

		// extender daily stats
		computeStatsExtender(stats, context, conn)

		// network daily stats
		computeStatsNetwork(stats, context, conn)

		// device daily stats
		computeStatsDevice(stats, context, conn)

		// all transfer
		computeStatsTransfer(stats, context, conn)

		// all packets
		computeStatsPackets(stats, context, conn)

		// extender transfer
		computeStatsExtenderTransfer(stats, context, conn)

	})

	// fixme
	return nil

/*

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
*/
}

func computeStatsProvider(stats *Stats, context context.Context, conn bringyour.PgConn) {
	result, err := conn.Query(
		context,
		`
			SELECT
				t.day AS day,
				t.device_id AS device_id,
				audit_provider_event.event_id AS event_id,
				audit_provider_event.event_type AS event_type,
				audit_provider_event.country_name AS country_name,
				audit_provider_event.region_name AS region_name,
				audit_provider_event.city_name AS city_name
			FROM (
				SELECT
					to_char(event_time, 'YYYY-MM-DD') AS day,
					device_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_provider_event
				WHERE
					now() - interval '1 days' * @lookback <= event_time AND
					event_type IN (
						@eventTypeProviderOffline,
						@eventTypeProviderOnlineSuperspeed,
						@eventTypeProviderOnlineNotSuperspeed
					)
				GROUP BY day, device_id

				UNION ALL

				SELECT
					'0000-00-00' AS day,
					device_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_provider_event
				WHERE
					event_time < now() - interval '1 days' * @lookback AND
					event_type IN (
						@eventTypeProviderOffline,
						@eventTypeProviderOnlineSuperspeed,
						@eventTypeProviderOnlineNotSuperspeed
					)
				GROUP BY device_id
			) t
			INNER JOIN audit_provider_event ON t.max_event_id::uuid = audit_provider_event.event_id
		`,
		bringyour.PgNamedArgs{
			"lookback": stats.Lookback,
			"eventTypeProviderOffline": AuditEventTypeProviderOffline,
			"eventTypeProviderOnlineSuperspeed": AuditEventTypeProviderOnlineSuperspeed,
			"eventTypeProviderOnlineNotSuperspeed": AuditEventTypeProviderOnlineNotSuperspeed,
		},
	)
	bringyour.With(result, err, func() {
		// fixme parse
	})
}

func computeStatsExtender(stats *Stats, context context.Context, conn bringyour.PgConn) {
	result, err := conn.Query(
		context,
		`
			SELECT
				t.day AS day,
				t.extender_id AS extender_id,
				audit_extender_event.event_id AS event_id,
				audit_extender_event.event_type AS event_type
			FROM (
				SELECT
					to_char(event_time, 'YYYY-MM-DD') AS day,
					extender_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_extender_event
				WHERE
					now() - interval '1 days' * @lookback <= event_time AND
					event_type IN (
						@eventTypeExtenderOffline,
						@eventTypeExtenderOnlineSuperspeed,
						@eventTypeExtenderOnlineNotSuperspeed
					)
				GROUP BY day, extender_id

				UNION ALL

				SELECT
					'0000-00-00' AS day,
					extender_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_extender_event
				WHERE
					event_time < now() - interval '1 days' * @lookback AND
					event_type IN (
						@eventTypeExtenderOffline,
						@eventTypeExtenderOnlineSuperspeed,
						@eventTypeExtenderOnlineNotSuperspeed
					)
				GROUP BY extender_id
		`,
		bringyour.PgNamedArgs{
			"lookback": stats.Lookback,
			"eventTypeExtenderOffline": AuditEventTypeExtenderOffline,
			"eventTypeExtenderOnlineSuperspeed": AuditEventTypeExtenderOnlineSuperspeed,
			"eventTypeExtenderOnlineNotSuperspeed": AuditEventTypeExtenderOnlineNotSuperspeed,
		},
	)
	bringyour.With(result, err, func() {
		// fixme parse
	})
}

func computeStatsNetwork(stats *Stats, context context.Context, conn bringyour.PgConn) {
	result, err := conn.Query(
		context,
		`
			SELECT
				t.day,
				t.network_id,
				audit_network_event.event_id,
				audit_network_event.event_type
			FROM (
				SELECT
					to_char(event_time, 'YYYY-MM-DD') AS day,
					network_id, MAX(event_id::varchar) AS max_event_id
				FROM audit_network_event
				WHERE
					now() - interval '1 days' * @lookback <= event_time AND
					event_type IN (
						@eventTypeNetworkCreated,
						@eventTypeNetworkDeleted
					)
				GROUP BY day, network_id

				UNION ALL

				SELECT
					'0000-00-00' AS day,
					network_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_network_event
				WHERE
					event_time < now() - interval '1 days' * @lookback AND
					event_type IN (
						@eventTypeNetworkCreated,
						@eventTypeNetworkDeleted
					)
				GROUP BY network_id
			) t
			INNER JOIN audit_network_event ON t.max_event_id::uuid = audit_network_event.event_id
		`,
		bringyour.PgNamedArgs{
			"lookback": stats.Lookback,
			"eventTypeNetworkCreated": AuditEventTypeNetworkCreated,
			"eventTypeNetworkDeleted": AuditEventTypeNetworkDeleted,
		},
	)
	bringyour.With(result, err, func() {
		// fixme parse
	})
}

func computeStatsDevice(stats *Stats, context context.Context, conn bringyour.PgConn) {
	result, err := conn.Query(
		context,
		`
			SELECT
				t.day AS day,
				t.device_id AS device_id,
				audit_device_event.event_id AS event_id,
				audit_device_event.event_type AS event_type
			FROM (
				SELECT
					to_char(event_time, 'YYYY-MM-DD') AS day,
					device_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_device_event
				WHERE
					now() - interval '1 days' * @lookback <= event_time AND
					event_type IN (
						@eventTypeDeviceAdded,
						@eventTypeDeviceRemoved
					)
				GROUP BY day, device_id

				UNION ALL

				SELECT
					'0000-00-00' AS day,
					device_id,
					MAX(event_id::varchar) AS max_event_id
				FROM audit_device_event
				WHERE
					event_time < now() - interval '1 days' * @lookback AND
					event_type IN (
						@eventTypeDeviceAdded,
						@eventTypeDeviceRemoved
					)
				GROUP BY device_id
			) t
			INNER JOIN audit_device_event ON t.max_event_id::uuid = audit_device_event.event_id
		`,
		bringyour.PgNamedArgs{
			"lookback": stats.Lookback,
			"eventTypeDeviceAdded": AuditEventTypeDeviceAdded,
			"eventTypeDeviceRemoved": AuditEventTypeDeviceRemoved,
		},
	)
	bringyour.With(result, err, func() {
		// fixme parse
	})
}

func computeStatsTransfer(stats *Stats, context context.Context, conn bringyour.PgConn) {
	result, err := conn.Query(
		context,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				SUM(transfer_bytes) AS net_transfer_bytes
			FROM audit_contract_event
			WHERE
				now() - interval '1 days' * @lookback <= event_time AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				'0000-00-00' AS day,
				SUM(transfer_bytes) AS net_transfer_bytes
			FROM audit_contract_event
			WHERE
				event_time < now() - interval '1 days' * @lookback AND
				event_type IN (@eventTypeContractClosedSuccess)

		`,
		bringyour.PgNamedArgs{
			"lookback": stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	bringyour.With(result, err, func() {
		// fixme parse
	})
}

func computeStatsPackets(stats *Stats, context context.Context, conn bringyour.PgConn) {
	result, err := conn.Query(
		context,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				SUM(transfer_packets) AS net_transfer_packets
			FROM audit_contract_event
			WHERE
				event_time <= now() AND
				now() - interval '1 days' * @lookback <= event_time AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				'0000-00-00' AS day,
				SUM(transfer_packets) AS net_transfer_packets
			FROM audit_contract_event
			WHERE
				event_time < now() - interval '1 days' * @lookback AND
				event_type IN (@eventTypeContractClosedSuccess)
		`,
		bringyour.PgNamedArgs{
			"lookback": stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	bringyour.With(result, err, func() {
		// fixme parse
	})
}

func computeStatsExtenderTransfer(stats *Stats, context context.Context, conn bringyour.PgConn) {
	result, err := conn.Query(
		context,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				SUM(transfer_bytes) AS net_transfer_bytes
			FROM audit_contract_event
			WHERE
				now() - interval '1 days' * @lookback <= event_time AND
				extender_id IS NOT NULL AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				'0000-00-00' AS day,
				SUM(transfer_bytes) AS net_transfer_bytes
			FROM audit_contract_event
			WHERE
				event_time < now() - interval '1 days' * @lookback AND
				event_type IN (@eventTypeContractClosedSuccess)
		`,
		bringyour.PgNamedArgs{
			"lookback": stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	bringyour.With(result, err, func() {
		// fixme parse
	})
}



func ExportStats(lookback int, stats Stats) {
	statsJson, err := json.Marshal(stats)
	if err != nil {
		panic(err)
	}

	bringyour.Redis(func(context context.Context, client bringyour.RedisClient) {
		client.Set(context, fmt.Sprintf("stats.last-%d", lookback), statsJson, 0)
	})
}

func GetExportedStatsJson(lookback int) *string {
	var statsJson *string
	bringyour.Redis(func(context context.Context, client bringyour.RedisClient) {
		value, err := client.Get(context, fmt.Sprint("stats.last-%d", lookback)).Result()
		if err != nil {
			statsJson = nil
		} else {
			statsJson = &value
		}
	})
	return statsJson
}

func GetExportedStats(lookback int) *Stats {
	statsJson := GetExportedStatsJson(lookback)
	if statsJson == nil {
		return nil
	}

	var stats Stats
	err := json.NewDecoder(strings.NewReader(*statsJson)).Decode(&stats)
    if err != nil {
        return nil
    }
    return &stats
}





// fixme
/*
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
*/






