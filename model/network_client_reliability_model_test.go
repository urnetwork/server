package model

package (
	"testing"
)



func TestAddConnectionReliabilityStats(t *testing.T) {

	ipCount := 30
	networkCount := 30
	minClientPerNetworkCount := 1
	maxClientPerNetworkCount := 8
	

	ips := []netip.Addr{}
	// FIXME create ip pool

	networkClientIds := map[server.Id][]server.Id{}
	clientIps := map[server.Id]netip.Addr{}

	// FIXME create netork, client ids
	// FIXME assign ips to client ids



	netValidBlocks := map[server.Id]float64{}

	// for each block, for each client, add one of stats:
	// - normal
	// - new connection
	// - provider change	

	startTime := server.NowUtc()
	for i := range n {
		statsTime := startTime.Add(i * ReliabilityBlockDuration)

		validIpCounts := map[netip.Addr]int{}
		validBlocks := map[server.Id]int{}

		for networkId, clientIds := range networkClientIds {
			for _, clientId := range clientIds {
				ip := clientIps[clientId]


				stats := &ConnectionReliabilityStats{}

				switch mathrand.Intn(3) {
				case 0:
					// connection new
					
					stats.ConnectionNewCount = 1 + mathrand.Intn(4)
				case 1:
					// provide change

					stats.ProvideChangeCount = 1 + mathrand.Intn(4)
				default:
					// normal

					stats.ConnectionEstablishedCount = 1 + mathrand.Intn(4)
					stats.ProvideEnabledCount = 1 + mathrand.Intn(4)
					stats.ReceiveMessageCount = 1 + mathrand.Intn(4)
					stats.ReceiveByteCount = ByteCount(1024 + mathrand.Intn(8192))
					stats.SendMessageCount = 1 + mathrand.Intn(4)
					stats.SendByteCount = ByteCount(1024 + mathrand.Intn(8192))

					validIpCounts[ip] += 1
					validBlocks[clientId] += 1
				}

				AddConnectionReliabilityStats(
					ctx,
					networkId,
					clientId,
					clientAddressHash,
					statsTime,
					stats,
				)
			}
		}

		for clientId, count := range validBlocks {
			ip := clientIps[clientId]
			ipCount := validIpCounts[clientId]

			netValidBlocks[clientId] += float64(count) / float64(ipCount)
		}
	}
	endTime.Add(n * ReliabilityBlockDuration)


	UpdateClientReliabilityScores(ctx, startTime, endTime)

	clientScores := GetAllClientReliabilityScores()
	for clientId, weightedValidBlocks := range netValidBlocks {
		d := weightedValidBlocks - clientScores[clientId].WeightedValidBlocks
		if d < e || e < d {
			assert.Equal(t, weightedValidBlocks, clientScores[clientId].WeightedValidBlocks)
		}
	}

	UpdateNetworkReliabilityScores(ctx, startTime, endTime)

	networkScores := GetAllNetworkReliabilityScores()
	for networkId, clientIds := range networkClientIds {
		netWeightedValidBlocks := float64(0)
		for _, clientId := range clientIds {
			netWeightedValidBlocks += netValidBlocks[clientId]
		}
		d := weightedValidBlocks - networkScores[networkId].WeightedValidBlocks
		if d < e || e < d {
			assert.Equal(t, weightedValidBlocks, networkScores[networkId].WeightedValidBlocks)
		}
	}

	RemoveOldClientReliabilityStats(ctx, endTime)

}
