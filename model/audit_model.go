package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/server/v2025"
)

type AuditEventType = string

const (
	AuditEventTypeProviderOffline             AuditEventType = "provider_offline"
	AuditEventTypeProviderOnlineSuperspeed    AuditEventType = "provider_online_superspeed"
	AuditEventTypeProviderOnlineNotSuperspeed AuditEventType = "provider_online_not_superspeed"
	AuditEventTypeExtenderOffline             AuditEventType = "extender_offline"
	AuditEventTypeExtenderOnlineSuperspeed    AuditEventType = "extender_online_superspeed"
	AuditEventTypeExtenderOnlineNotSuperspeed AuditEventType = "extender_online_not_superspeed"
	AuditEventTypeNetworkCreated              AuditEventType = "network_created"
	AuditEventTypeNetworkDeleted              AuditEventType = "network_deleted"
	AuditEventTypeDeviceAdded                 AuditEventType = "device_added"
	AuditEventTypeDeviceRemoved               AuditEventType = "device_removed"
	AuditEventTypeContractClosedSuccess       AuditEventType = "contract_closed_success"
	AuditEventTypeCirclePayoutFailed          AuditEventType = "circle_payout_failed"
)

type Stats struct {
	Lookback    int   `json:"lookback"`
	CreatedTime int64 `json:"created_time"`

	AllTransferData        map[string]int `json:"all_transfer_data"`
	AllTransferSummary     int            `json:"all_transfer_summary"`
	AllTransferSummaryRate int            `json:"all_transfer_summary_rate"`

	AllPacketsData        map[string]int `json:"all_packets_data"`
	AllPacketsSummary     int            `json:"all_packets_summary"`
	AllPacketsSummaryRate int            `json:"all_packets_summary_rate"`

	ProvidersData              map[string]int `json:"providers_data"`
	ProvidersSuperspeedData    map[string]int `json:"providers_superspeed_data"`
	ProvidersSummary           int            `json:"providers_summary"`
	ProvidersSummarySuperspeed int            `json:"providers_summary_superspeed"`

	CountriesData    map[string]int `json:"countries_data"`
	CountriesSummary int            `json:"countries_summary"`

	RegionsData    map[string]int `json:"regions_data"`
	RegionsSummary int            `json:"regions_summary"`

	CitiesData    map[string]int `json:"cities_data"`
	CitiesSummary int            `json:"cities_summary"`

	ExtenderTransferData        map[string]int `json:"extender_transfer_data"`
	ExtenderTransferSummary     int            `json:"extender_transfer_summary"`
	ExtenderTransferSummaryRate int            `json:"extender_transfer_summary_rate"`

	ExtendersData              map[string]int `json:"extenders_data"`
	ExtendersSuperspeedData    map[string]int `json:"extenders_superspeed_data"`
	ExtendersSummary           int            `json:"extenders_summary"`
	ExtendersSummarySuperspeed int            `json:"extenders_summary_superspeed"`

	NetworksData    map[string]int `json:"networks_data"`
	NetworksSummary int            `json:"networks_summary"`

	DevicesData    map[string]int `json:"devices_data"`
	DevicesSummary int            `json:"devices_summary"`

	// internal data that is not exported to json

	// deviceId -> *ProviderState
	activeProviders map[server.Id]*ProviderState
	// extenderId -> *ExtenderState
	activeExtenders map[server.Id]*ExtenderState
	// networkId -> bool
	activeNetworks map[server.Id]bool
	// deviceId -> bool
	activeDevices map[server.Id]bool
}

type ProviderState struct {
	networkId   server.Id
	superspeed  bool
	countryName string
	regionName  string
	cityName    string
}

type ExtenderState struct {
	networkId  server.Id
	superspeed bool
}

// 90 is the standard lookback used in the api
func ComputeStats90(ctx context.Context) *Stats {
	return ComputeStats(ctx, 90)
}

func ComputeStats(ctx context.Context, lookback int) *Stats {
	stats := &Stats{
		Lookback:    lookback,
		CreatedTime: server.NowUtc().UnixMilli(),
	}

	server.Db(ctx, func(conn server.PgConn) {
		glog.Infof("[audit]ComputeStats90 computeStatsProvider\n")
		// provider daily stats + cities, regions, countries
		computeStatsProvider(ctx, stats, conn)

		glog.Infof("[audit]ComputeStats90 computeStatsExtender\n")
		// extender daily stats
		computeStatsExtender(ctx, stats, conn)

		glog.Infof("[audit]ComputeStats90 computeStatsNetwork\n")
		// network daily stats
		computeStatsNetwork(ctx, stats, conn)

		glog.Infof("[audit]ComputeStats90 computeStatsDevice\n")
		// device daily stats
		computeStatsDevice(ctx, stats, conn)

		glog.Infof("[audit]ComputeStats90 computeStatsTransfer\n")
		// all transfer
		computeStatsTransfer(ctx, stats, conn)

		glog.Infof("[audit]ComputeStats90 computeStatsPackets\n")
		// all packets
		computeStatsPackets(ctx, stats, conn)

		glog.Infof("[audit]ComputeStats90 computeStatsExtenderTransfer\n")
		// extender transfer
		computeStatsExtenderTransfer(ctx, stats, conn)
	})

	return stats
}

func computeStatsProvider(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			SELECT
				t.day,
				t.device_id,
				audit_provider_event.network_id,
				audit_provider_event.event_type,
				audit_provider_event.country_name,
				audit_provider_event.region_name,
				audit_provider_event.city_name
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
		server.PgNamedArgs{
			"startDay":                             startDay,
			"lookback":                             stats.Lookback,
			"eventTypeProviderOffline":             AuditEventTypeProviderOffline,
			"eventTypeProviderOnlineSuperspeed":    AuditEventTypeProviderOnlineSuperspeed,
			"eventTypeProviderOnlineNotSuperspeed": AuditEventTypeProviderOnlineNotSuperspeed,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		activeProviders := map[server.Id]*ProviderState{}

		providersData := map[string]int{}
		providersSuperspeedData := map[string]int{}
		countriesData := map[string]int{}
		regionsData := map[string]int{}
		citiesData := map[string]int{}

		exportActive := func() {
			providersSuperspeed := map[server.Id]bool{}
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
		var deviceId server.Id
		var networkId server.Id
		var eventType string
		var countryName string
		var regionName string
		var cityName string
		for result.Next() {
			result.Scan(&day, &deviceId, &networkId, &eventType, &countryName, &regionName, &cityName)

			// server.Logger().Printf("FOUND GEO \"%s\" \"%s\" \"%s\"\n", countryName, regionName, cityName)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					// server.Logger().Printf("%s <> %s\n", packDay, day)
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers
			switch AuditEventType(eventType) {
			case AuditEventTypeProviderOffline:
				delete(activeProviders, deviceId)
			case AuditEventTypeProviderOnlineSuperspeed:
				providerState := &ProviderState{
					networkId:   networkId,
					superspeed:  true,
					countryName: countryName,
					regionName:  regionName,
					cityName:    cityName,
				}
				activeProviders[deviceId] = providerState
			case AuditEventTypeProviderOnlineNotSuperspeed:
				providerState := &ProviderState{
					networkId:   networkId,
					superspeed:  false,
					countryName: countryName,
					regionName:  regionName,
					cityName:    cityName,
				}
				activeProviders[deviceId] = providerState
			}
		}
		exportActive()
		for packDay := nextDay(activeDay); packDay < endDay; packDay = nextDay(activeDay) {
			// server.Logger().Printf("%s <> %s\n", packDay, endDay)
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

		stats.activeProviders = activeProviders
	})
}

func computeStatsExtender(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			SELECT
				t.day,
				t.extender_id,
				audit_extender_event.network_id,
				audit_extender_event.event_type
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
		server.PgNamedArgs{
			"startDay":                             startDay,
			"lookback":                             stats.Lookback,
			"eventTypeExtenderOffline":             AuditEventTypeExtenderOffline,
			"eventTypeExtenderOnlineSuperspeed":    AuditEventTypeExtenderOnlineSuperspeed,
			"eventTypeExtenderOnlineNotSuperspeed": AuditEventTypeExtenderOnlineNotSuperspeed,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		activeExtenders := map[server.Id]*ExtenderState{}

		extendersData := map[string]int{}
		extendersSuperspeedData := map[string]int{}

		exportActive := func() {
			extendersSuperspeed := map[server.Id]bool{}
			for extenderId, extenderState := range activeExtenders {
				if extenderState.superspeed {
					extendersSuperspeed[extenderId] = true
				}
			}

			extendersData[activeDay] = len(activeExtenders)
			extendersSuperspeedData[activeDay] = len(extendersSuperspeed)
		}

		var day string
		var extenderId server.Id
		var networkId server.Id
		var eventType string
		for result.Next() {
			result.Scan(&day, &extenderId, &networkId, &eventType)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers
			switch AuditEventType(eventType) {
			case AuditEventTypeExtenderOffline:
				delete(activeExtenders, extenderId)
			case AuditEventTypeExtenderOnlineSuperspeed:
				providerState := &ExtenderState{
					networkId:  networkId,
					superspeed: true,
				}
				activeExtenders[extenderId] = providerState
			case AuditEventTypeProviderOnlineNotSuperspeed:
				providerState := &ExtenderState{
					networkId:  networkId,
					superspeed: false,
				}
				activeExtenders[extenderId] = providerState
			}
		}
		exportActive()
		for packDay := nextDay(activeDay); packDay < endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			exportActive()
		}

		stats.ExtendersData = extendersData
		stats.ExtendersSuperspeedData = extendersSuperspeedData
		stats.ExtendersSummary = summary(extendersData)
		stats.ExtendersSummarySuperspeed = summary(extendersSuperspeedData)

		stats.activeExtenders = activeExtenders
	})
}

func computeStatsNetwork(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			SELECT
				t.day,
				t.network_id,
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
		server.PgNamedArgs{
			"startDay":                startDay,
			"lookback":                stats.Lookback,
			"eventTypeNetworkCreated": AuditEventTypeNetworkCreated,
			"eventTypeNetworkDeleted": AuditEventTypeNetworkDeleted,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		activeNetworks := map[server.Id]bool{}

		networksData := map[string]int{}

		exportActive := func() {
			networksData[activeDay] = len(activeNetworks)
		}

		var day string
		var networkId server.Id
		var eventType string
		for result.Next() {
			result.Scan(&day, &networkId, &eventType)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers
			switch AuditEventType(eventType) {
			case AuditEventTypeNetworkDeleted:
				delete(activeNetworks, networkId)
			case AuditEventTypeNetworkCreated:
				activeNetworks[networkId] = true
			}
		}
		exportActive()
		for packDay := nextDay(activeDay); packDay < endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			exportActive()
		}

		stats.NetworksData = networksData
		stats.NetworksSummary = summary(networksData)

		stats.activeNetworks = activeNetworks
	})
}

func computeStatsDevice(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			SELECT
				t.day,
				t.device_id,
				audit_device_event.event_type
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
		server.PgNamedArgs{
			"startDay":               startDay,
			"lookback":               stats.Lookback,
			"eventTypeDeviceAdded":   AuditEventTypeDeviceAdded,
			"eventTypeDeviceRemoved": AuditEventTypeDeviceRemoved,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		activeDevices := map[server.Id]bool{}

		devicesData := map[string]int{}

		exportActive := func() {
			devicesData[activeDay] = len(activeDevices)
		}

		var day string
		var deviceId server.Id
		var eventType string
		for result.Next() {
			result.Scan(&day, &deviceId, &eventType)

			if day != activeDay {
				exportActive()
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					activeDay = packDay
					exportActive()
				}
				activeDay = day
			}

			// update the active providers
			switch AuditEventType(eventType) {
			case AuditEventTypeDeviceRemoved:
				delete(activeDevices, deviceId)
			case AuditEventTypeDeviceAdded:
				activeDevices[deviceId] = true
			}
		}
		exportActive()
		for packDay := nextDay(activeDay); packDay < endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			exportActive()
		}

		stats.DevicesData = devicesData
		stats.DevicesSummary = summary(devicesData)

		stats.activeDevices = activeDevices
	})
}

func computeStatsTransfer(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				COALESCE(SUM(transfer_byte_count), 0) AS net_transfer_byte_count
			FROM audit_contract_event
			WHERE
				now() - interval '1 days' * @lookback < event_time AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				@startDay AS day,
				COALESCE(SUM(transfer_byte_count), 0) AS net_transfer_byte_count
			FROM audit_contract_event
			WHERE
				event_time BETWEEN now() - interval '1 days' * (@lookback + 1) AND now() - interval '1 days' * @lookback AND
				to_char(event_time, 'YYYY-MM-DD') = @startDay AND
				event_type IN (@eventTypeContractClosedSuccess)

			ORDER BY day ASC
		`,
		server.PgNamedArgs{
			"startDay":                       startDay,
			"lookback":                       stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		allTransferData := map[string]int{}

		var day string
		var netTransferBytes int
		for result.Next() {
			result.Scan(&day, &netTransferBytes)

			if day != activeDay {
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					activeDay = packDay
					allTransferData[activeDay] = 0
				}
				activeDay = day
			}

			allTransferData[activeDay] += netTransferBytes
		}
		for packDay := nextDay(activeDay); packDay < endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			allTransferData[activeDay] = 0
		}

		allTransferSummary := summary(allTransferData)
		stats.AllTransferData = allTransferData
		// TiB
		stats.AllTransferSummary = int(math.Round(float64(allTransferSummary) / float64(1024*1024)))
		// bytes to average gbps
		stats.AllTransferSummaryRate = int(math.Round(float64(8*allTransferSummary) / float64(1024*1024*60*60*24)))
	})
}

func computeStatsPackets(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
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
		server.PgNamedArgs{
			"startDay":                       startDay,
			"lookback":                       stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		allPacketsData := map[string]int{}

		var day string
		var netTransferPackets int
		for result.Next() {
			result.Scan(&day, &netTransferPackets)

			if day != activeDay {
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					activeDay = packDay
					allPacketsData[activeDay] = 0
				}
				activeDay = day
			}

			allPacketsData[activeDay] += netTransferPackets
		}
		for packDay := nextDay(activeDay); packDay < endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			allPacketsData[activeDay] = 0
		}

		allPacketsSummary := summary(allPacketsData)
		stats.AllPacketsData = allPacketsData
		// GiB
		stats.AllPacketsSummary = allPacketsSummary
		// packets to average pps
		stats.AllTransferSummaryRate = int(math.Round(float64(allPacketsSummary) / float64(60*60*24)))
	})
}

func computeStatsExtenderTransfer(ctx context.Context, stats *Stats, conn server.PgConn) {
	startDay, endDay := dayRange(stats.Lookback)
	result, err := conn.Query(
		ctx,
		`
			SELECT
				to_char(event_time, 'YYYY-MM-DD') AS day,
				COALESCE(SUM(transfer_byte_count), 0) AS net_transfer_byte_count
			FROM audit_contract_event
			WHERE
				now() - interval '1 days' * @lookback < event_time AND
				extender_id IS NOT NULL AND
				event_type IN (@eventTypeContractClosedSuccess)
			GROUP BY day

			UNION ALL

			SELECT
				@startDay AS day,
				COALESCE(SUM(transfer_byte_count), 0) AS net_transfer_byte_count
			FROM audit_contract_event
			WHERE
				event_time BETWEEN now() - interval '1 days' * (@lookback + 1) AND now() - interval '1 days' * @lookback AND
				to_char(event_time, 'YYYY-MM-DD') = @startDay AND
				extender_id IS NOT NULL AND
				event_type IN (@eventTypeContractClosedSuccess)

			ORDER BY day ASC
		`,
		server.PgNamedArgs{
			"startDay":                       startDay,
			"lookback":                       stats.Lookback,
			"eventTypeContractClosedSuccess": AuditEventTypeContractClosedSuccess,
		},
	)
	server.WithPgResult(result, err, func() {
		activeDay := startDay
		extenderTransferData := map[string]int{}

		var day string
		var netTransferBytes int
		for result.Next() {
			result.Scan(&day, &netTransferBytes)

			if day != activeDay {
				for packDay := nextDay(activeDay); packDay < day; packDay = nextDay(activeDay) {
					activeDay = packDay
					extenderTransferData[activeDay] = 0
				}
				activeDay = day
			}

			extenderTransferData[activeDay] += netTransferBytes
		}
		for packDay := nextDay(activeDay); packDay < endDay; packDay = nextDay(activeDay) {
			activeDay = packDay
			extenderTransferData[activeDay] = 0
		}

		extenderTransferSummary := summary(extenderTransferData)
		stats.ExtenderTransferData = extenderTransferData
		// GiB
		stats.ExtenderTransferSummary = int(math.Round(float64(extenderTransferSummary) / float64(1024*1024)))
		// bytes to average gbps
		stats.ExtenderTransferSummaryRate = int(math.Round(float64(8*extenderTransferSummary) / float64(1024*1024*60*60*24)))
	})
}

func summary(data map[string]int) int {
	k := 3
	days := maps.Keys(data)
	sort.Strings(days)
	summaryDays := days[max(0, len(days)-k):]
	maxValue := 0
	for i := 0; i < len(summaryDays); i += 1 {
		day := summaryDays[i]
		maxValue = max(maxValue, data[day])
	}
	return maxValue
}

func dayRange(lookback int) (string, string) {
	// this should be running in the same tz as postgres
	end := server.NowUtc().Local()
	d, err := time.ParseDuration(fmt.Sprintf("-%dh", lookback*24))
	server.Raise(err)
	start := end.Add(d)

	return start.Format("2006-01-02"), end.Format("2006-01-02")
}

func nextDay(day string) string {
	start, err := time.Parse("2006-01-02", day)
	server.Raise(err)
	d, err := time.ParseDuration("24h")
	server.Raise(err)
	end := start.Add(d)
	return end.Format("2006-01-02")
}

func ExportStats(ctx context.Context, stats *Stats) {
	statsJson, err := json.Marshal(stats)
	server.Raise(err)

	server.Redis(ctx, func(client server.RedisClient) {
		_, err := client.Set(
			ctx,
			fmt.Sprintf("stats.last-%d", stats.Lookback),
			statsJson,
			0,
		).Result()
		server.Raise(err)
	})
}

func GetExportedStatsJson(ctx context.Context, lookback int) *string {
	var statsJson *string
	server.Redis(ctx, func(client server.RedisClient) {
		var value string
		var err error
		value, err = client.Get(
			ctx,
			fmt.Sprintf("stats.last-%d", lookback),
		).Result()
		if err == nil {
			statsJson = &value
		}
	})
	return statsJson
}

func GetExportedStats(ctx context.Context, lookback int) *Stats {
	statsJson := GetExportedStatsJson(ctx, lookback)
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

type AuditEvent struct {
	EventId      server.Id
	EventTime    time.Time
	EventType    AuditEventType
	EventDetails *string
}

type AuditProviderEvent struct {
	AuditEvent

	NetworkId   server.Id
	DeviceId    server.Id
	CountryName string
	RegionName  string
	CityName    string
}

func NewAuditProviderEvent(eventType AuditEventType) *AuditProviderEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditProviderEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

type AuditExtenderEvent struct {
	AuditEvent

	NetworkId  server.Id
	ExtenderId server.Id
}

func NewAuditExtenderEvent(eventType AuditEventType) *AuditExtenderEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditExtenderEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

type AuditNetworkEvent struct {
	AuditEvent

	NetworkId server.Id
}

func NewAuditNetworkEvent(eventType AuditEventType) *AuditNetworkEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditNetworkEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

type AuditDeviceEvent struct {
	AuditEvent

	NetworkId server.Id
	DeviceId  server.Id
}

func NewAuditDeviceEvent(eventType AuditEventType) *AuditDeviceEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditDeviceEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

type AuditContractEvent struct {
	AuditEvent

	ContractId        server.Id
	ClientNetworkId   server.Id
	ClientDeviceId    server.Id
	ProviderNetworkId server.Id
	ProviderDeviceId  server.Id
	ExtenderNetworkId *server.Id
	ExtenderId        *server.Id
	TransferBytes     int64
	TransferPackets   int64
}

func NewAuditContractEvent(eventType AuditEventType) *AuditContractEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditContractEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
		TransferBytes:   0,
		TransferPackets: 0,
	}
}

func AddAuditEvent(ctx context.Context, event interface{}) {
	switch v := event.(type) {
	case *AuditProviderEvent:
		AddAuditProviderEvent(ctx, v)
	case *AuditExtenderEvent:
		AddAuditExtenderEvent(ctx, v)
	case *AuditNetworkEvent:
		AddAuditNetworkEvent(ctx, v)
	case *AuditDeviceEvent:
		AddAuditDeviceEvent(ctx, v)
	case *AuditContractEvent:
		AddAuditContractEvent(ctx, v)
	default:
		panic(fmt.Sprintf("Event type not recognized: %T", v))
	}
}

func AddAuditProviderEvent(ctx context.Context, event *AuditProviderEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_provider_event
			(
				event_id,
				event_time,
				network_id,
				device_id,
				event_type,
				event_details,
				country_name,
				region_name,
				city_name
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			`,
			event.EventId,
			event.EventTime,
			event.NetworkId,
			event.DeviceId,
			event.EventType,
			event.EventDetails,
			event.CountryName,
			event.RegionName,
			event.CityName,
		)
		server.Raise(err)
	})
}

func AddAuditExtenderEvent(ctx context.Context, event *AuditExtenderEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_extender_event
			(
				event_id,
				event_time,
				network_id,
				extender_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			`,
			event.EventId,
			event.EventTime,
			event.NetworkId,
			event.ExtenderId,
			event.EventType,
			event.EventDetails,
		)
		server.Raise(err)
	})
}

func AddAuditNetworkEvent(ctx context.Context, event *AuditNetworkEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_network_event
			(
				event_id,
				event_time,
				network_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5)
			`,
			event.EventId,
			event.EventTime,
			event.NetworkId,
			event.EventType,
			event.EventDetails,
		)
		server.Raise(err)
	})
}

func AddAuditDeviceEvent(ctx context.Context, event *AuditDeviceEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_device_event
			(
				event_id,
				event_time,
				network_id,
				device_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5, $6)
			`,
			event.EventId,
			event.EventTime,
			event.NetworkId,
			event.DeviceId,
			event.EventType,
			event.EventDetails,
		)
		server.Raise(err)
	})
}

func AddAuditContractEvent(ctx context.Context, event *AuditContractEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_contract_event
			(
				event_id,
				event_time,
				contract_id,
				client_network_id,
				client_device_id,
				provider_network_id,
				provider_device_id,
				extender_network_id,
				extender_id,
				event_type,
				event_details,
				transfer_byte_count,
				transfer_packets
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			`,
			event.EventId,
			event.EventTime,
			event.ContractId,
			event.ClientNetworkId,
			event.ClientDeviceId,
			event.ProviderNetworkId,
			event.ProviderDeviceId,
			event.ExtenderNetworkId,
			event.ExtenderId,
			event.EventType,
			event.EventDetails,
			event.TransferBytes,
			event.TransferPackets,
		)
		server.Raise(err)
	})
}

type AuditAccountPaymentEvent struct {
	AuditEvent

	AccountPaymentId server.Id
}

func NewAuditAccountPaymentEvent(eventType AuditEventType) *AuditAccountPaymentEvent {
	eventId := server.NewId()
	eventTime := server.NowUtc()
	return &AuditAccountPaymentEvent{
		AuditEvent: AuditEvent{
			EventId:   eventId,
			EventTime: eventTime,
			EventType: eventType,
		},
	}
}

func AddAuditAccountPaymentEvent(ctx context.Context, event *AuditAccountPaymentEvent) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
			INSERT INTO audit_account_payment
			(
				event_id,
				event_time,
				payment_id,
				event_type,
				event_details
			)
			VALUES ($1, $2, $3, $4, $5)
			`,
			event.EventId,
			event.EventTime,
			event.AccountPaymentId,
			event.EventType,
			event.EventDetails,
		)
		server.Raise(err)
	})
}
