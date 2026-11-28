// The connect service's timer census (server timer_census.go, CPUOPT7.md
// §11): the server's own sites, and the connect library's and quic-go's,
// whose hooks sample their sites and record into the same counter under the
// client_*, quic_* and http3_* site labels.
package connect

import (
	"time"

	"github.com/urnetwork/connect/v2026"
	quic "github.com/urnetwork/quic-go/v2026"

	"github.com/urnetwork/server/v2026"
)

// The server site of each of the connect library's.
var clientTimerCensusSites = [connect.TimerCensusSiteCount]server.TimerCensusSite{
	connect.TimerCensusSiteTimer:                 server.TimerCensusSiteClientTimer,
	connect.TimerCensusSiteRouteWriteTimer:       server.TimerCensusSiteClientRouteWriteTimer,
	connect.TimerCensusSiteReceiveHandoffTimer:   server.TimerCensusSiteClientReceiveHandoffTimer,
	connect.TimerCensusSiteReceiveDeliveryTimer:  server.TimerCensusSiteClientReceiveDeliveryTimer,
	connect.TimerCensusSiteAckCompressTimer:      server.TimerCensusSiteClientAckCompressTimer,
	connect.TimerCensusSitePackWaitTimer:         server.TimerCensusSiteClientPackWaitTimer,
	connect.TimerCensusSiteWindowPacingTimer:     server.TimerCensusSiteClientWindowPacingTimer,
	connect.TimerCensusSiteSequenceDeadlineTimer: server.TimerCensusSiteClientSequenceDeadlineTimer,
}

// The server site of each of quic-go's.
var quicTimerCensusSites = [quic.TimerCensusSiteCount]server.TimerCensusSite{
	quic.TimerCensusSiteStreamReadTimer:  server.TimerCensusSiteQuicStreamReadTimer,
	quic.TimerCensusSiteStreamWriteTimer: server.TimerCensusSiteQuicStreamWriteTimer,
	quic.TimerCensusSiteConnTimer:        server.TimerCensusSiteQuicConnTimer,
	quic.TimerCensusSiteHTTP3IdleTimer:   server.TimerCensusSiteHttp3IdleTimer,
}

// Starts the process's census: the server's sites, and the connect library's
// and quic-go's hooks with the same sampling. Returns the stop, which removes
// the hooks. A non-positive SamplePeriod leaves all of it off.
func startTimerCensus(settings *server.TimerCensusSettings) (func(), error) {
	stopServerCensus, err := server.StartTimerCensus(settings)
	if err != nil {
		return nil, err
	}
	if settings.SamplePeriod <= 0 {
		return stopServerCensus, nil
	}
	connect.SetTimerCensus(&connect.TimerCensus{
		SamplePeriod: settings.SamplePeriod,
		Record: func(site connect.TimerCensusSite, horizon time.Duration, stop bool, count int) {
			server.RecordTimerChange(clientTimerCensusSites[site], horizon, stop, count)
		},
	})
	quic.SetTimerCensus(&quic.TimerCensus{
		SamplePeriod: settings.SamplePeriod,
		Record: func(site quic.TimerCensusSite, horizon time.Duration, stop bool, count int) {
			server.RecordTimerChange(quicTimerCensusSites[site], horizon, stop, count)
		},
	})
	return func() {
		connect.SetTimerCensus(nil)
		quic.SetTimerCensus(nil)
		stopServerCensus()
	}, nil
}
