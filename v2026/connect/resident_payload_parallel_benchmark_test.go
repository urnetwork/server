package connect

import (
	"fmt"
	"sync"
	"testing"
)

func BenchmarkResidentPayloadParallel(b *testing.B) {
	for _, workers := range []int{32, 96} {
		for _, spread := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				name := "off"
				var ledger *residentPayloadLedger
				if enabled {
					name = "on"
					ledger = &residentPayloadLedger{}
				}
				b.Run(fmt.Sprintf("producers%d/spread%t/%s", workers, spread, name), func(b *testing.B) {
					b.StopTimer()
					var ready, done sync.WaitGroup
					ready.Add(workers)
					done.Add(workers)
					start := make(chan struct{})
					charge := residentPayloadCharge{1, 1200, 2048}
					for worker := 0; worker < workers; worker++ {
						owner := byte(0)
						if spread {
							owner = byte(worker)
						}
						count := b.N / workers
						if worker < b.N%workers {
							count++
						}
						go func() {
							defer done.Done()
							ready.Done()
							<-start
							for range count {
								if ledger != nil {
									ledger.update(residentPayloadForwardIngress, owner, charge, true)
									ledger.update(residentPayloadForwardIngress, owner, charge, false)
								}
							}
						}()
					}
					ready.Wait()
					b.ReportAllocs()
					b.ResetTimer()
					b.StartTimer()
					close(start)
					done.Wait()
					b.StopTimer()
					if ledger != nil {
						s := ledger.snapshot()
						if !s.Complete || s.Groups[1].Messages != 0 || s.Groups[1].Admitted != s.Groups[1].Released {
							b.Fatal("conservation")
						}
					}
				})
			}
		}
	}
}
