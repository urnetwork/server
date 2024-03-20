package model

import (
	"time"

	mathrand "math/rand"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/session"
)


type StatsProvidersOverviewArgs struct {
	Lookback int `json:"lookback"`
}

type StatsProvidersOverviewResult struct {
	Lookback int `json:"lookback"`
	CreatedTime time.Time `json:"created_time"`
	Uptime map[string]float64 `json:"uptime"`
	TransferData map[string]float64 `json:"transfer_data"`
	Payout map[string]float64 `json:"payout"`
	SearchInterest map[string]int `json:"search_interest"`
	Contracts map[string]int `json:"contracts"`
	Clients map[string]int `json:"clients"`
}


func StatsProvidersOverviewLast90(
	clientSession *session.ClientSession,
) (*StatsProvidersOverviewResult, error) {
	return StatsProvidersOverview(
		&StatsProvidersOverviewArgs{
			Lookback: 90,
		},
		clientSession,
	)
}


func StatsProvidersOverview(
	providersOverview *StatsProvidersOverviewArgs,
	clientSession *session.ClientSession,
) (*StatsProvidersOverviewResult, error) {
	// TODO
	// create random statistics for the last `lookback` days


	days := lookbackDays(providersOverview.Lookback)

	uptime := map[string]float64{}
	for _, day := range days {
		uptime[day] = 24 * mathrand.Float64()
	}

	transferData := map[string]float64{}
	for _, day := range days {
		transferData[day] = 1024 * mathrand.Float64()
	}

	payout := map[string]float64{}
	for _, day := range days {
		payout[day] = 5 * mathrand.Float64()
	}

	searchInterest := map[string]int{}
	for _, day := range days {
		searchInterest[day] = mathrand.Intn(300)
	}

	contracts := map[string]int{}
	for _, day := range days {
		contracts[day] = mathrand.Intn(5000)
	}

	clients := map[string]int{}
	for _, day := range days {
		clients[day] = mathrand.Intn(100)
	}

	result := &StatsProvidersOverviewResult{
		Lookback: providersOverview.Lookback,
		CreatedTime: bringyour.NowUtc(),
		Uptime: uptime,
		TransferData: transferData,
		Payout: payout,
		SearchInterest: searchInterest,
		Contracts: contracts,
		Clients: clients,
	}

	return result, nil
}


type StatsProvidersResult struct {
	CreatedTime time.Time `json:"created_time"`
	Providers []*ProviderStats `json:"providers"`
}

type ProviderStats struct {
	ClientId bringyour.Id `json:"client_id"`
	Connected bool `json:"connected"`
	ConnectedEventsLast24h []*ConnectedEvent `json:"connected_events_last_24h"`
	UptimeLast24h float64 `json:"uptime_last_24h"`
	TransferDataLast24h float64 `json:"transfer_data_last_24h"`
	PayoutLast24h float64 `json:"payout_last_24h"`
	SearchInterestLast24h int `json:"search_interest_last_24h"`
	ContractsLast24h int `json:"contracts_last_24h"`
	ClientsLast24h int `json:"clients_last_24h"`
	ProvideMode int `json:"provide_mode"`
}

type ConnectedEvent struct {
	EventTime time.Time `json:"event_time"`
	Connected bool `json:"connected"`
}


func StatsProviders(
	clientSession *session.ClientSession,
) (*StatsProvidersResult, error) {
	// TODO
	// create random statistics

	providers := []*ProviderStats{}

	for i := 0; i < 16; i += 1 {
		clientId := bringyour.NewId()
		

		connectedEvents := []*ConnectedEvent{}
		endTime := bringyour.NowUtc()
		t := endTime.Add(-24 * time.Hour)
		for t.Before(endTime) {
			connected := (len(connectedEvents) % 2 == 1)
			connectedEvents = append(connectedEvents, &ConnectedEvent{
				EventTime: t,
				Connected: connected,
			})
			t = t.Add(time.Duration(mathrand.Intn(120)) * time.Minute)
		}


		provider := &ProviderStats{
			ClientId: clientId,
			Connected: true,
			ConnectedEventsLast24h: connectedEvents,
			UptimeLast24h: 24 * mathrand.Float64(),
			TransferDataLast24h: 1024 * mathrand.Float64(),
			PayoutLast24h: 5 * mathrand.Float64(),
			SearchInterestLast24h: mathrand.Intn(300),
			ContractsLast24h: mathrand.Intn(5000),
			ClientsLast24h: mathrand.Intn(100),
			ProvideMode: ProvideModePublic,
		}
		providers = append(providers, provider)
	}

	result := &StatsProvidersResult{
		CreatedTime: bringyour.NowUtc(),
		Providers: providers,
	}

	return result, nil
}


type StatsProviderArgs struct {
    ClientId bringyour.Id `json:"client_id"`
    Lookback int `json:"lookback,omitempty"`
}

type StatsProviderResult struct {
	Lookback int `json:"lookback"`
	CreatedTime time.Time `json:"created_time"`
	Uptime map[string]float64 `json:"uptime"`
	TransferData map[string]float64 `json:"transfer_data"`
	Payout map[string]float64 `json:"payout"`
	SearchInterest map[string]int `json:"search_interest"`
	Contracts map[string]int `json:"contracts"`
	Clients map[string]int `json:"clients"`
	ClientDetails []*ClientDetail `json:"client_details"`
}

type ClientDetail struct {
	ClientId bringyour.Id `json:"client_id"`
	TransferData map[string]float64 `json:"transfer_data"`
}


func StatsProviderLast90(
	provider *StatsProviderArgs,
	clientSession *session.ClientSession,
) (*StatsProviderResult, error) {
	return StatsProvider(
		&StatsProviderArgs{
			ClientId: provider.ClientId,
			Lookback: 90,
		},
		clientSession,
	)
}


func StatsProvider(
	provider *StatsProviderArgs,
	clientSession *session.ClientSession,
) (*StatsProviderResult, error) {
	// TODO
	// create random statistics for the last `lookback` days

	days := lookbackDays(provider.Lookback)

	uptime := map[string]float64{}
	for _, day := range days {
		uptime[day] = 24 * mathrand.Float64()
	}

	transferData := map[string]float64{}
	for _, day := range days {
		transferData[day] = 1024 * mathrand.Float64()
	}

	payout := map[string]float64{}
	for _, day := range days {
		payout[day] = 5 * mathrand.Float64()
	}

	searchInterest := map[string]int{}
	for _, day := range days {
		searchInterest[day] = mathrand.Intn(300)
	}

	contracts := map[string]int{}
	for _, day := range days {
		contracts[day] = mathrand.Intn(5000)
	}

	clients := map[string]int{}
	for _, day := range days {
		clients[day] = mathrand.Intn(100)
	}

	clientDetails := []*ClientDetail{}
	n := mathrand.Intn(100)
	for i := 0; i < n; i += 1 {
		clientId := bringyour.NewId()
		clientTransferData := map[string]float64{}
		for _, day := range days {
			clientTransferData[day] = 64 * mathrand.Float64()
		}
		clientDetail := &ClientDetail{
			ClientId: clientId,
			TransferData: clientTransferData,
		}
		clientDetails = append(clientDetails, clientDetail)
	}

	result := &StatsProviderResult{
		Lookback: provider.Lookback,
		CreatedTime: bringyour.NowUtc(),
		Uptime: uptime,
		TransferData: transferData,
		Payout: payout,
		SearchInterest: searchInterest,
		Contracts: contracts,
		Clients: clients,
		ClientDetails: clientDetails,
	}

	return result, nil
}


// yyyy-mm-dd
func lookbackDays(lookback int) []string {
	days := []string{}
	t := bringyour.NowUtc().UTC()
	for i := 0; i < lookback; i += 1 {
		days = append(days, t.Format("2006-01-02"))
		t = t.Add(-24 * time.Hour)
	}
	return days
}

