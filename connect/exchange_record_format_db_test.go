package connect

import (
	"bytes"
	"context"
	"encoding/gob"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// An exchange's nomination writes the resident record in the format of its
// settings: gob, which a reader from before CPUOPT7 §12 reads, unless
// TypedResidentRecords is set. The model reads either.
func TestExchangeNominationWritesTheResidentRecordFormatOfItsSettings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, typed := range []bool{false, true} {
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				exchangeSettings := DefaultExchangeSettings()
				exchangeSettings.KeyEventDelivery.Enabled = false
				exchangeSettings.TypedResidentRecords = typed
				exchange := NewExchange(
					ctx,
					"host0",
					"test",
					"test",
					map[int]int{},
					map[string]string{"host0": "127.0.0.1"},
					exchangeSettings,
				)
				defer func() {
					exchange.Close()
					if !exchange.WaitForIdle(ctx) {
						t.Error("the exchange did not join its residents")
					}
				}()
				networkId, userId := server.NewId(), server.NewId()
				networkName := "residentRecordFormatGob"
				if typed {
					networkName = "residentRecordFormatTyped"
				}
				model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
				clientId := server.NewId()
				model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
				instanceId := server.NewId()

				if !exchange.NominateLocalResidentWithContext(ctx, clientId, instanceId, nil) {
					t.Fatal("the resident was not nominated")
				}
				record := model.Testing_ResidentRecord(ctx, clientId)
				if len(record) == 0 {
					t.Fatal("no resident record")
				}
				// a typed record starts with a byte no gob stream starts with
				gobStart := record[0] < 0x80 || 0xf8 <= record[0]
				if gobStart == typed {
					t.Fatalf("TypedResidentRecords=%t wrote a record starting %#x", typed, record[0])
				}
				var old model.NetworkClientResident
				oldErr := gob.NewDecoder(bytes.NewBuffer(record)).Decode(&old)
				if (oldErr == nil) == typed {
					t.Fatalf("TypedResidentRecords=%t: an old reader's decode = %v", typed, oldErr)
				}
				resident := model.GetResidentForClient(ctx, clientId, 0)
				if resident == nil || resident.ClientId != clientId || resident.InstanceId != instanceId || resident.ResidentService != "test" || resident.ResidentHost != "host0" {
					t.Fatalf("TypedResidentRecords=%t: resident = %+v", typed, resident)
				}
			}()
		}
	})
}
