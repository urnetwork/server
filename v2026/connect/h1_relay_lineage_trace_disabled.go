//go:build !acklineagetrace

package connect

import "github.com/urnetwork/server/v2026"

// Every call site is guarded by this constant. Normal builds neither inspect
// messages nor allocate metadata, read clocks, or install an observer.
const h1RelayLineageTraceEnabled = false

type h1RelayLineageSpan struct{}

func beginH1RelayLineage(string, server.Id, server.Id, []byte, int, int) h1RelayLineageSpan {
	return h1RelayLineageSpan{}
}

func (h1RelayLineageSpan) end(string, bool, int, int) {}

func beginH1RelayLineageBatch(string, server.Id, server.Id, [][]byte) []h1RelayLineageSpan {
	return nil
}
func endH1RelayLineageBatch([]h1RelayLineageSpan, string, bool) {}
