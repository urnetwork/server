package model

import (
	"context"
	"fmt"
	"encoding/json"
	"strings"
	"time"
	"math"

	"golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
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
	Lookback int `json:"lookback"`
	CreatedTime int64 `json:"createdTime"`

	AllTransferData map[string]int `json:"allTransferData"`
    AllTransferSummary int `json:"allTransferSummary"`
    AllTransferSummaryRate int `json:"allTransferSummaryRate"`

    AllPacketsData map[string]int `json:"allPacketsData"`
    AllPacketsSummary int `json:"allPacketsSummary"`
    AllPacketsSummaryRate int `json:"allPacketsSummaryRate"`

    ProvidersData map[string]int `json:"providersData"`
    ProvidersSuperspeedData map[string]int `json:"providersSuperspeedData"`
    ProvidersSummary int `json:"providersSummary"`
    ProvidersSummarySuperspeed int `json:"providersSummarySuperspeed"`

    CountriesData map[string]int `json:"countriesData"`
    CountriesSummary int `json:"countriesSummary"`

    RegionsData map[string]int `json:"regionsData"`
    RegionsSummary int `json:"regionsSummary"`

    CitiesData map[string]int `json:"citiesData"`
    CitiesSummary int `json:"citiesSummary"`

    ExtenderTransferData map[string]int `json:"extenderTransferData"`
    ExtenderTransferSummary int `json:"extenderTransferSummary"`
    ExtenderTransferSummaryRate int `json:"extenderTransferSummaryRate"`

    ExtendersData map[string]int `json:"extendersData"`
    ExtendersSuperspeedData map[string]int `json:"extendersSuperspeedData"`
    ExtendersSummary int `json:"extendersSummary"`
    ExtendersSummarySuperspeed int `json:"extendersSummarySuperspeed"`

    NetworksData map[string]int `json:"networksData"`
    NetworksSummary int `json:"networksSummary"`

    DevicesData map[string]int `json:"devicesData"`
    DevicesSummary int `json:"devicesSummary"`

    // internal data that is not exported to json

    // deviceId -> *ProviderState
    ActiveProviders map[ulid.ULID]*ProviderState
    // extenderId -> *ExtenderState
    ActiveExtenders map[ulid.ULID]*ExtenderState
    // networkId -> bool
    ActiveNetworks map[ulid.ULID]bool
    // deviceId -> bool
    ActiveDevices map[ulid.ULID]bool
}

type ProviderState struct {
	networkId ulid.ULID
	superspeed bool
	countryName string
	regionName string
	cityName string
}

type ExtenderState struct {
	networkId ulid.ULID
	superspeed bool
}


func ComputeStats(lookback int) *Stats {
	stats := &Stats{
		Lookback: lookback,
		CreatedTime: time.Now().UnixMilli(),
	}

	bringyour.Db(func (context context.Context, conn bringyour.PgConn) {
		bringyour.Logger().Printf("ComputeStats90 computeStatsProvider\n")
		// provider daily stats + cities, regions, countries
		computeStatsProvider(stats, context, conn)

		bringyour.Logger().Printf("ComputeStats90 computeStatsExtender\n")
		// extender daily stats
		computeStatsExtender(stats, context, conn)

		bringyour.Logger().Printf("ComputeStats90 computeStatsNetwork\n")
		// network daily stats
		computeStatsNetwork(stats, context, conn)

		bringyour.Logger().Printf("ComputeStats90 computeStatsDevice\n")
		// device daily stats
		computeStatsDevice(stats, context, conn)

		bringyour.Logger().Printf("ComputeStats90 computeStatsTransfer\n")
		// all transfer
		computeStatsTransfer(stats, context, conn)

		bringyour.Logger().Printf("ComputeStats90 computeStatsPackets\n")
		// all packets
		computeStatsPackets(stats, context, conn)

		bringyour.Logger().Printf("ComputeStats90 computeStatsExtenderTransfer\n")
		// extender transfer
		computeStatsExtenderTransfer(stats, context, conn)
	})

	return stats
}

func computeStatsProvider(stats *Stats, context context.Context, conn bringyour.PgConn) {
    startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		context,
		`
			SELECT
				t.day AS day,
				t.device_id AS device_id,
				audit_provider_event.network_id AS network_id,
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
					@startDay AS day,
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
			ORDER BY day ASC
		`,
		bringyour.PgNamedArgs{
			"startDay": startDay,
			"lookback": stats.Lookback,
			"eventTypeProviderOffline": AuditEventTypeProviderOffline,
			"eventTypeProviderOnlineSuperspeed": AuditEventTypeProviderOnlineSuperspeed,
			"eventTypeProviderOnlineNotSuperspeed": AuditEventTypeProviderOnlineNotSuperspeed,
		},
	)
	bringyour.With(result, err, func() {
		activeDay := startDay
		activeProviders := map[ulid.ULID]*ProviderState{}

		providersData := map[string]int{}
		providersSuperspeedData := map[string]int{}
		countriesData := map[string]int{}
		regionsData := map[string]int{}
		citiesData := map[string]int{}

		exportActive := func() {
			providersSuperspeed := map[ulid.ULID]bool{}
			countryNames := map[string]bool{}
			regionNames := map[string]bool{}
			cityNames := map[string]bool{}
			for deviceId, providerState := range activeProviders {
				if providerState.superspeed {
					providersSuperspeed[deviceId] = true
				}
				countryNames[providerState.countryName] = true
				regionNames[providerState.regionName] = true
				cityNames[providerState.cityName] = true
			}

			providersData[activeDay] = len(activeProviders)
			providersSuperspeedData[activeDay] = len(providersSuperspeed)
			countriesData[activeDay] = len(countryNames)
			regionsData[activeDay] = len(regionNames)
			citiesData[activeDay] = len(cityNames)
		}

		var day string
		var deviceIdPg bringyour.PgUUID
		var networkIdPg bringyour.PgUUID
		var eventType string
		var countryName string
		var regionName string
		var cityName string
		for result.Next() {
			result.Scan(&day, &deviceIdPg, &networkIdPg, &eventType, &countryName, &regionName, &cityName)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay != day; packDay = nextDay(activeDay) {
					bringyour.Logger().Printf("%s <> %s\n", packDay, day)
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers
			deviceId := *ulid.FromPg(deviceIdPg)
			switch AuditEventType(eventType) {
			case AuditEventTypeProviderOffline:
				delete(activeProviders, deviceId)
			case AuditEventTypeProviderOnlineSuperspeed:
				providerState := &ProviderState{
					networkId: *ulid.FromPg(networkIdPg),
					superspeed: true,
					countryName: countryName,
					regionName: regionName,
					cityName: cityName,
				}
				activeProviders[deviceId] = providerState
			case AuditEventTypeProviderOnlineNotSuperspeed:
				providerState := &ProviderState{
					networkId: *ulid.FromPg(networkIdPg),
					superspeed: false,
					countryName: countryName,
					regionName: regionName,
					cityName: cityName,
				}
				activeProviders[deviceId] = providerState
			}
		}
		exportActive()
		for packDay := nextDay(activeDay); packDay != endDay; packDay = nextDay(activeDay) {
			bringyour.Logger().Printf("%s <> %s\n", packDay, endDay)
			activeDay = packDay
			exportActive()
		}

	    stats.ProvidersData = providersData
	    stats.ProvidersSuperspeedData = providersSuperspeedData
	    stats.ProvidersSummary = summary(providersData)
	    stats.ProvidersSummarySuperspeed = summary(providersSuperspeedData)
		stats.CountriesData = countriesData
		stats.CountriesSummary = summary(countriesData)
		stats.RegionsData = regionsData
		stats.RegionsSummary = summary(regionsData)
		stats.CitiesData = citiesData
		stats.CitiesSummary = summary(citiesData)

		stats.ActiveProviders = activeProviders
	})
}

func computeStatsExtender(stats *Stats, context context.Context, conn bringyour.PgConn) {
    startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		context,
		`
			SELECT
				t.day AS day,
				t.extender_id AS extender_id,
				audit_extender_event.network_id AS network_id,
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
					@startDay AS day,
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
			) t
			INNER JOIN audit_extender_event ON t.max_event_id::uuid = audit_extender_event.event_id
			ORDER BY day ASC
		`,
		bringyour.PgNamedArgs{
			"startDay": startDay,
			"lookback": stats.Lookback,
			"eventTypeExtenderOffline": AuditEventTypeExtenderOffline,
			"eventTypeExtenderOnlineSuperspeed": AuditEventTypeExtenderOnlineSuperspeed,
			"eventTypeExtenderOnlineNotSuperspeed": AuditEventTypeExtenderOnlineNotSuperspeed,
		},
	)
	bringyour.With(result, err, func() {
		activeDay := startDay
		activeExtenders := map[ulid.ULID]*ExtenderState{}

		extendersData := map[string]int{}
		extendersSuperspeedData := map[string]int{}

		exportActive := func() {
			extendersSuperspeed := map[ulid.ULID]bool{}
			for extenderId, extenderState := range activeExtenders {
				if extenderState.superspeed {
					extendersSuperspeed[extenderId] = true
				}
			}

			extendersData[activeDay] = len(activeExtenders)
			extendersSuperspeedData[activeDay] = len(extendersSuperspeed)
		}

		var day string
		var extenderIdPg bringyour.PgUUID
		var networkIdPg bringyour.PgUUID
		var eventType string
		for result.Next() {
			result.Scan(&day, &extenderIdPg, &networkIdPg, &eventType)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay != day; packDay = nextDay(activeDay) {
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers
			extenderId := *ulid.FromPg(extenderIdPg)
			switch AuditEventType(eventType) {
			case AuditEventTypeExtenderOffline:
				delete(activeExtenders, extenderId)
			case AuditEventTypeExtenderOnlineSuperspeed:
				providerState := &ExtenderState{
					networkId: *ulid.FromPg(networkIdPg),
					superspeed: true,
				}
				activeExtenders[extenderId] = providerState
			case AuditEventTypeProviderOnlineNotSuperspeed:
				providerState := &ExtenderState{
					networkId: *ulid.FromPg(networkIdPg),
					superspeed: false,
				}
				activeExtenders[extenderId] = providerState
			}
		}
		exportActive()
		for packDay := nextDay(activeDay); packDay != endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			exportActive()
		}

	    stats.ExtendersData = extendersData
	    stats.ExtendersSuperspeedData = extendersSuperspeedData
	    stats.ExtendersSummary = summary(extendersData)
	    stats.ExtendersSummarySuperspeed = summary(extendersSuperspeedData)

	    stats.ActiveExtenders = activeExtenders
	})
}

func computeStatsNetwork(stats *Stats, context context.Context, conn bringyour.PgConn) {
    startDay, endDay := dayRange(stats.Lookback)
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
					@startDay AS day,
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
			ORDER BY day ASC
		`,
		bringyour.PgNamedArgs{
			"startDay": startDay,
			"lookback": stats.Lookback,
			"eventTypeNetworkCreated": AuditEventTypeNetworkCreated,
			"eventTypeNetworkDeleted": AuditEventTypeNetworkDeleted,
		},
	)
	bringyour.With(result, err, func() {
		activeDay := startDay
		activeNetworks := map[ulid.ULID]bool{}

		networksData := map[string]int{}

		exportActive := func() {
			networksData[activeDay] = len(activeNetworks)
		}

		var day string
		var networkIdPg bringyour.PgUUID
		var eventType string
		for result.Next() {
			result.Scan(&day, &networkIdPg, &eventType)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay != day; packDay = nextDay(activeDay) {
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers
			networkId := *ulid.FromPg(networkIdPg)
			switch AuditEventType(eventType) {
			case AuditEventTypeNetworkDeleted:
				delete(activeNetworks, networkId)
			case AuditEventTypeNetworkCreated:
				activeNetworks[networkId] = true
			}
		}
		exportActive()
		for packDay := nextDay(activeDay); packDay != endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			exportActive()
		}

	    stats.NetworksData = networksData
	    stats.NetworksSummary = summary(networksData)

	    stats.ActiveNetworks = activeNetworks
	})
}

func computeStatsDevice(stats *Stats, context context.Context, conn bringyour.PgConn) {
    startDay, endDay := dayRange(stats.Lookback)
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
					@startDay AS day,
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
			ORDER BY day ASC
		`,
		bringyour.PgNamedArgs{
			"startDay": startDay,
			"lookback": stats.Lookback,
			"eventTypeDeviceAdded": AuditEventTypeDeviceAdded,
			"eventTypeDeviceRemoved": AuditEventTypeDeviceRemoved,
		},
	)
	bringyour.With(result, err, func() {
		activeDay := startDay
		activeDevices := map[ulid.ULID]bool{}

		devicesData := map[string]int{}

		exportActive := func() {
			devicesData[activeDay] = len(activeDevices)
		}

		var day string
		var deviceIdPg bringyour.PgUUID
		var eventType string
		for result.Next() {
			result.Scan(&day, &deviceIdPg, &eventType)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay != day; packDay = nextDay(activeDay) {
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers
			deviceId := *ulid.FromPg(deviceIdPg)
			switch AuditEventType(eventType) {
			case AuditEventTypeDeviceRemoved:
				delete(activeDevices, deviceId)
			case AuditEventTypeDeviceAdded:
				activeDevices[deviceId] = true
			}
		}
		exportActive()
		for packDay := nextDay(activeDay); packDay != endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			exportActive()
		}

	    stats.DevicesData = devicesData
	    stats.DevicesSummary = summary(devicesData)

	    stats.ActiveDevices = activeDevices
	})
}

func computeStatsTransfer(stats *Stats, context context.Context, conn bringyour.PgConn) {
    startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		context,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				COALESCE(SUM(transfer_bytes), 0) AS net_transfer_bytes
			FROM audit_contract_event
			WHERE
				now() - interval '1 days' * @lookback < event_time AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				@startDay AS day,
				COALESCE(SUM(transfer_bytes), 0) AS net_transfer_bytes
			FROM audit_contract_event
			WHERE
				event_time BETWEEN now() - interval '1 days' * (@lookback + 1) AND now() - interval '1 days' * @lookback AND
				to_char(event_time, 'YYYY-MM-DD') = @startDay AND
				event_type IN (@eventTypeContractClosedSuccess)

			ORDER BY day ASC
		`,
		bringyour.PgNamedArgs{
			"startDay": startDay,
			"lookback": stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	bringyour.With(result, err, func() {
		activeDay := startDay
		allTransferData := map[string]int{}

		var day string
		var netTransferBytes int
		for result.Next() {
			result.Scan(&day, &netTransferBytes)

			if day != activeDay {
				for packDay := nextDay(activeDay); packDay != day; packDay = nextDay(activeDay) {
					activeDay = packDay
					allTransferData[activeDay] = 0
				}
				activeDay = day
			}

			allTransferData[activeDay] += netTransferBytes
		}
		for packDay := nextDay(activeDay); packDay != endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			allTransferData[activeDay] = 0
		}

    	allTransferSummary := summary(allTransferData)
	    stats.AllTransferData = allTransferData
	    // TiB
	    stats.AllTransferSummary = int(math.Round(float64(allTransferSummary) / float64(1024 * 1024)))
	    // bytes to average gbps
	    stats.AllTransferSummaryRate = int(math.Round(float64(8 * allTransferSummary) / float64(1024 * 1024 * 60 * 60 * 24)))
	})
}

func computeStatsPackets(stats *Stats, context context.Context, conn bringyour.PgConn) {
    startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		context,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				COALESCE(SUM(transfer_packets), 0) AS net_transfer_packets
			FROM audit_contract_event
			WHERE
				now() - interval '1 days' * @lookback < event_time AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				@startDay AS day,
				COALESCE(SUM(transfer_packets), 0) AS net_transfer_packets
			FROM audit_contract_event
			WHERE
				event_time BETWEEN now() - interval '1 days' * (@lookback + 1) AND now() - interval '1 days' * @lookback AND
				to_char(event_time, 'YYYY-MM-DD') = @startDay AND
				event_type IN (@eventTypeContractClosedSuccess)

			ORDER BY day ASC
		`,
		bringyour.PgNamedArgs{
			"startDay": startDay,
			"lookback": stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	bringyour.With(result, err, func() {
		activeDay := startDay
		allPacketsData := map[string]int{}

		var day string
		var netTransferPackets int
		for result.Next() {
			result.Scan(&day, &netTransferPackets)

			if day != activeDay {
				for packDay := nextDay(activeDay); packDay != day; packDay = nextDay(activeDay) {
					activeDay = packDay
					allPacketsData[activeDay] = 0
				}
				activeDay = day
			}

			allPacketsData[activeDay] += netTransferPackets
		}
		for packDay := nextDay(activeDay); packDay != endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			allPacketsData[activeDay] = 0
		}

    	allPacketsSummary := summary(allPacketsData)
	    stats.AllPacketsData = allPacketsData
	    // GiB
	    stats.AllPacketsSummary = allPacketsSummary
	    // packets to average pps
	    stats.AllTransferSummaryRate = int(math.Round(float64(allPacketsSummary) / float64(60 * 60 * 24)))
	})
}

func computeStatsExtenderTransfer(stats *Stats, context context.Context, conn bringyour.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		context,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				COALESCE(SUM(transfer_bytes), 0) AS net_transfer_bytes
			FROM audit_contract_event
			WHERE
				now() - interval '1 days' * @lookback < event_time AND
				extender_id IS NOT NULL AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				@startDay AS day,
				COALESCE(SUM(transfer_bytes), 0) AS net_transfer_bytes
			FROM audit_contract_event
			WHERE
				event_time BETWEEN now() - interval '1 days' * (@lookback + 1) AND now() - interval '1 days' * @lookback AND
				to_char(event_time, 'YYYY-MM-DD') = @startDay AND
				extender_id IS NOT NULL AND
				event_type IN (@eventTypeContractClosedSuccess)

			ORDER BY day ASC
		`,
		bringyour.PgNamedArgs{
			"startDay": startDay,
			"lookback": stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	bringyour.With(result, err, func() {
		activeDay := startDay
		extenderTransferData := map[string]int{}

		var day string
		var netTransferBytes int
		for result.Next() {
			result.Scan(&day, &netTransferBytes)

			if day != activeDay {
				for packDay := nextDay(activeDay); packDay != day; packDay = nextDay(activeDay) {
					activeDay = packDay
					extenderTransferData[activeDay] = 0
				}
				activeDay = day
			}

			extenderTransferData[activeDay] += netTransferBytes
		}
		for packDay := nextDay(activeDay); packDay != endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			extenderTransferData[activeDay] = 0
		}

    	extenderTransferSummary := summary(extenderTransferData)
	    stats.ExtenderTransferData = extenderTransferData
	    // GiB
	    stats.ExtenderTransferSummary = int(math.Round(float64(extenderTransferSummary) / float64(1024 * 1024)))
	    // bytes to average gbps
	    stats.ExtenderTransferSummaryRate = int(math.Round(float64(8 * extenderTransferSummary) / float64(1024 * 1024 * 60 * 60 * 24)))
	})
}


func summary(data map[string]int) int {
	k := 3
	days := maps.Keys(data)
	summaryDays := days[bringyour.MaxInt(0, len(days) - k):]
	values := make([]int, len(summaryDays))
	for i := 0; i < len(summaryDays); i += 1 {
		day := summaryDays[i]
		values[i] = data[day]
	}
	return bringyour.MaxInt(values...)
}

func dayRange(lookback int) (string, string) {
	// this should be running in the same tz as postgres
	end := time.Now().Local()
	d, err := time.ParseDuration(fmt.Sprintf("-%dh", lookback * 24))
	bringyour.Raise(err)
	start := end.Add(d)

	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

func nextDay(day string) string {
	start, err := time.Parse("2006-01-02", day)
	bringyour.Raise(err)
	d, err := time.ParseDuration("24h")
	bringyour.Raise(err)
	end := start.Add(d)
	return end.Format("2006-01-02")
}



func ExportStats(stats *Stats) {
	statsJson, err := json.Marshal(stats)
	bringyour.Raise(err)

	bringyour.Redis(func(context context.Context, client bringyour.RedisClient) {
		_, err := client.Set(
			context,
			fmt.Sprintf("stats.last-%d", stats.Lookback),
			statsJson,
			0,
		).Result()
		bringyour.Raise(err)
	})
}

func GetExportedStatsJson(lookback int) *string {
	var statsJson *string
	bringyour.Redis(func(context context.Context, client bringyour.RedisClient) {
		var value string
		var err error
		value, err = client.Get(
			context,
			fmt.Sprintf("stats.last-%d", lookback),
		).Result()
		if err == nil {
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
    	// junk stats, ignore
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






