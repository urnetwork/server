package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"net/netip"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/jwt"
	"github.com/urnetwork/server/v2025/session"
)

func TestAddClientReliabilityStats(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		ipCount := 30
		networkCount := 30
		minClientPerNetworkCount := 1
		maxClientPerNetworkCount := 8

		ips := []netip.Addr{}
		for range ipCount {
			ipv4 := make([]byte, 4)
			mathrand.Read(ipv4)
			ip, ok := netip.AddrFromSlice(ipv4)
			assert.Equal(t, ok, true)
			ips = append(ips, ip)
		}

		networkClientIds := map[server.Id][]server.Id{}
		clientIps := map[server.Id]netip.Addr{}

		totalClientCount := 0
		for i := range networkCount {
			networkId := server.NewId()
			clientCount := minClientPerNetworkCount
			if d := maxClientPerNetworkCount - minClientPerNetworkCount; 0 < d {
				clientCount += mathrand.Intn(d)
			}
			for j := range clientCount {
				clientId := server.NewId()
				ip := ips[mathrand.Intn(len(ips))]

				networkClientIds[networkId] = append(networkClientIds[networkId], clientId)
				clientIps[clientId] = ip

				// connect the client
				Testing_CreateDevice(
					ctx,
					networkId,
					server.NewId(),
					clientId,
					"",
					"",
				)
				clientAddress := "127.0.0.1:20000"
				handlerId := server.NewId()
				connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
				assert.Equal(t, err, nil)
				location := &Location{
					LocationType: "",
					City:         fmt.Sprintf("foo%d", j),
					Region:       fmt.Sprintf("bar%d", i),
					Country:      "United States",
					CountryCode:  "us",
				}
				CreateLocation(ctx, location)
				connectionLocationScores := &ConnectionLocationScores{}
				err = SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)

				// fmt.Printf("init client_id[%d] %s\n", totalClientCount, clientId)

				totalClientCount += 1
			}
		}

		netReliabilityScores := map[server.Id]float64{}
		netIndependentReliabilityScores := map[server.Id]float64{}

		// for each block, for each client, add one of stats:
		// - normal
		// - new connection
		// - provider change

		n := 128
		eps := float64(0.001)

		startTime := server.NowUtc()
		for i := range n {
			statsTime := startTime.Add(time.Duration(i) * ReliabilityBlockDuration)

			validIpCounts := map[[32]byte]int{}
			validBlocks := map[server.Id]int{}

			for networkId, clientIds := range networkClientIds {
				for _, clientId := range clientIds {
					ip := clientIps[clientId]
					clientAddressHash := server.ClientIpHashForAddr(ip)

					stats := &ClientReliabilityStats{}

					switch mathrand.Intn(3) {
					case 0:
						// connection new

						stats.ConnectionNewCount = uint64(1 + mathrand.Intn(4))
					case 1:
						// provide change

						stats.ProvideChangedCount = uint64(1 + mathrand.Intn(4))
					default:
						// normal

						stats.ConnectionEstablishedCount = uint64(1 + mathrand.Intn(4))
						stats.ProvideEnabledCount = uint64(1 + mathrand.Intn(4))
						stats.ReceiveMessageCount = uint64(1 + mathrand.Intn(4))
						stats.ReceiveByteCount = ByteCount(1024 + mathrand.Intn(8192))
						stats.SendMessageCount = uint64(1 + mathrand.Intn(4))
						stats.SendByteCount = ByteCount(1024 + mathrand.Intn(8192))

						validIpCounts[clientAddressHash] += 1
						validBlocks[clientId] += 1
					}

					AddClientReliabilityStats(
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
				clientAddressHash := server.ClientIpHashForAddr(ip)
				ipCount := validIpCounts[clientAddressHash]

				netReliabilityScores[clientId] += float64(count) / float64(ipCount)
				netIndependentReliabilityScores[clientId] += float64(count)
			}
		}
		endTime := startTime.Add(time.Duration(n) * ReliabilityBlockDuration)

		UpdateClientReliabilityScores(ctx, endTime, true)

		lookbackClientScores := GetAllClientReliabilityScores(ctx)
		clientScores := lookbackClientScores[3]
		for clientId, reliabilityScore := range netReliabilityScores {
			d := reliabilityScore - clientScores[clientId].ReliabilityScore
			if d < -eps || eps < d {
				assert.Equal(t, reliabilityScore, clientScores[clientId].ReliabilityScore)
			}
		}
		for clientId, indepententReliabilityScore := range netIndependentReliabilityScores {
			d := indepententReliabilityScore - clientScores[clientId].IndependentReliabilityScore
			if d < -eps || eps < d {
				assert.Equal(t, indepententReliabilityScore, clientScores[clientId].IndependentReliabilityScore)
			}
		}

		UpdateNetworkReliabilityScores(ctx, startTime, endTime, true)
		UpdateNetworkReliabilityWindow(ctx, startTime, endTime, true)

		blockCountPerBucket := ReliabilityBlockCountPerBucket()

		for _, networkScores := range []map[server.Id]ReliabilityScore{
			GetAllNetworkReliabilityScores(ctx),
			GetAllMultipliedNetworkReliabilityScores(ctx),
		} {
			for networkId, clientIds := range networkClientIds {
				netReliabilityScore := float64(0)
				netIndepententReliabilityScore := float64(0)
				for _, clientId := range clientIds {
					netReliabilityScore += netReliabilityScores[clientId]
					netIndepententReliabilityScore += netIndependentReliabilityScores[clientId]
				}
				d := netReliabilityScore - networkScores[networkId].ReliabilityScore
				if d < -eps || eps < d {
					assert.Equal(t, netReliabilityScore, networkScores[networkId].ReliabilityScore)
				}
				d = netIndepententReliabilityScore - networkScores[networkId].IndependentReliabilityScore
				if d < -eps || eps < d {
					assert.Equal(t, netIndepententReliabilityScore, networkScores[networkId].IndependentReliabilityScore)
				}

				isPro := false

				byJwt := jwt.NewByJwt(
					networkId,
					server.NewId(),
					"",
					false,
					isPro,
				).Client(clientIds[0], server.NewId())
				clientSession := session.Testing_CreateClientSession(
					ctx,
					byJwt,
				)
				clientSession.ClientAddress = "1.1.1.1:90000"

				reliabilityWindow, err := GetNetworkReliabilityWindow(clientSession)
				assert.Equal(t, err, nil)
				assert.Equal(t, reliabilityWindow.MaxTotalClientCount, len(clientIds))
				for _, totalClientCount := range reliabilityWindow.TotalClientCounts {
					assert.Equal(t, totalClientCount, len(clientIds))
				}
				// reconstruct the total score from the weight
				windowReliabilityScore := reliabilityWindow.MeanReliabilityWeight * float64(int(reliabilityWindow.MaxBucketNumber-reliabilityWindow.MinBucketNumber)*blockCountPerBucket)
				d = windowReliabilityScore - networkScores[networkId].ReliabilityScore
				if d < -eps || eps < d {
					assert.Equal(t, windowReliabilityScore, networkScores[networkId].ReliabilityScore)
				}
			}
		}

		RemoveOldClientReliabilityStats(ctx, endTime, 10000)

	})
}
