package connect

import (
	"time"
)

// Server-directed connection moves (MOVE.md). `ExchangeMoveSettings` holds the
// move primitives: intents, the resident handoff at the target, compulsory moves
// and drain moves. `ExchangeMoverSettings` holds the mover that decides balance
// moves from the Redis load model. Both are embedded in `ExchangeSettings`;
// production runs these defaults, and the Redis move control can narrow the
// mover at runtime but never widen it beyond them (MOVE.md §5.4).

type ExchangeMoveSettings struct {
	// The ceiling for every move behavior of this exchange. False: no move
	// intents, no handoff by intent, no moves sent, no load published, no mover.
	// Drains then use the legacy migrate.
	EnableMove bool
	// Drains send compulsory moves with intents to the same host (MOVE.md §5.6,
	// D13) instead of the legacy migrate. The Redis move control can turn this
	// off for the next drain without a deploy.
	EnableDrainMoves bool
	// How long a move's intent lives past its `migrate_time` (past the send
	// when the migrate time is already past): the client connect timeout plus
	// a margin. Measured from the migrate time, so a drain's jitter never eats
	// into it. A client that arrives after this forwards as today. The source's
	// poll of a balance move stops with it. Non-positive writes no intent, so
	// no move is sent.
	MoveArrivalTimeout time.Duration
	// How often the source re-reads `ncr_` for each of its outstanding balance
	// moves, to retire a resident the target took over (MOVE.md §5.3). Zero or
	// negative disables the poll, leaving the resident refresh as the only
	// retirement path.
	MoveIntentPollInterval time.Duration
	// The deadline of a compulsory move after its `migrate_time` (MOVE.md §5.6,
	// D12). The source closes the client's transports with reason "moved" when
	// its resident is still local at the deadline. A drain's deadlines are no
	// later than the drain's own. Non-positive closes at `migrate_time`.
	MoveCompulsoryTimeout time.Duration
	// Sent to the client as `prefer_duration`: how long it prefers the target
	// address for reconnects after a successful move (D10). Zero sends none.
	MovePreferDuration time.Duration
}

func DefaultExchangeMoveSettings() *ExchangeMoveSettings {
	return &ExchangeMoveSettings{
		EnableMove:             true,
		EnableDrainMoves:       true,
		MoveArrivalTimeout:     3 * time.Minute,
		MoveIntentPollInterval: 1 * time.Second,
		MoveCompulsoryTimeout:  60 * time.Second,
		MovePreferDuration:     6 * time.Hour,
	}
}

type ExchangeMoverSettings struct {
	// The mover's mode when the Redis move control is absent: "off", "dry_run"
	// or "on". The control can only narrow it, never widen past
	// `EnableMove`. Off by default, so a deploy changes nothing until an
	// operator sets the control.
	MoveDefaultMode string
	// Load record cadence into the `{connect_load}` hash. Non-positive
	// publishes nothing.
	MoveLoadPublishInterval time.Duration
	// Load records older than this are ignored; a host with no fresh record is
	// neither a source nor a target.
	MoveLoadStaleTimeout time.Duration
	// The movers delete load records older than this, left by processes that
	// ended without removing theirs (MOVE.md §5.4). Non-positive deletes none.
	MoveLoadCleanupTimeout time.Duration
	// Mover tick cadence. Non-positive runs no mover.
	MoveTickInterval time.Duration
	// Fleet budget of balance moves per minute (D8). Zero or negative allows none.
	MoveMaxPerMinute int
	// Per source host and per target host caps per minute. Zero or negative
	// allows none.
	MoveMaxPerMinutePerSourceHost int
	MoveMaxPerMinutePerTargetHost int
	// Moves one process may start in one tick, and the candidates it examines
	// in one tick. Non-positive moves none.
	MoveMaxPerTick int
	// The band around the fleet mean of residents per capacity unit (D3, D8): a
	// host above mean × (1 + tolerance) is a source.
	MoveBalanceTolerance float64
	// Optional extra source condition: host CPU busy fraction at or above this.
	// Zero disables it.
	MoveSourceMinCpu float64
	// Never move onto a host whose CPU busy fraction is at or above this.
	MoveTargetMaxCpu float64
	// Emergency shedding: compulsory moves of idle clients while host CPU is at
	// or above this for `MoveEmergencyTicks` consecutive ticks. Zero or
	// negative disables emergency shedding.
	MoveEmergencyCpu   float64
	MoveEmergencyTicks int
	// A balance-move candidate's every forward has been payload-idle this long
	// (D7).
	MoveIdleTimeout time.Duration
	// A balance-move candidate has been resident here at least this long (D7).
	MoveMinResidentAge time.Duration
	// Minimum time between balance moves of one client.
	MoveClientCooldown time.Duration
	// Move a client that just arrived on a source host before it has state
	// (D9). Off until idle moves prove out.
	EnableArrivalMoves bool
	// Pause the tick while this process's Postgres pool acquire wait exceeds
	// this. Zero or negative disables the guard.
	MoveDbWaitPauseThreshold time.Duration
	// Reload of the edge inventory from the vault `services.yml` (D17).
	// Non-positive loads it once.
	MoveInventoryRefreshInterval time.Duration
	// A target IP's breaker opens when failure reports over sends exceed this
	// ratio with at least `MoveTargetBreakerMinSends` sends in the window, and
	// stays open for `MoveTargetBreakerTimeout` (MOVE.md §5.5). A non-positive
	// minimum requires no minimum; a non-positive timeout never opens a breaker.
	MoveTargetBreakerRatio    float64
	MoveTargetBreakerMinSends int
	MoveTargetBreakerTimeout  time.Duration
	// The window a target IP's sends and failure reports are counted in,
	// starting with its first send. It must cover the client's connect timeout
	// (60 s), since a failure is reported only after it. Ten minutes holds the
	// minimum sample at moderate send rates. Non-positive counts no sends, so no
	// breaker opens.
	MoveTargetBreakerWindow time.Duration
}

func DefaultExchangeMoverSettings() *ExchangeMoverSettings {
	return &ExchangeMoverSettings{
		MoveDefaultMode:               "off",
		MoveLoadPublishInterval:       15 * time.Second,
		MoveLoadStaleTimeout:          60 * time.Second,
		MoveLoadCleanupTimeout:        10 * time.Minute,
		MoveTickInterval:              10 * time.Second,
		MoveMaxPerMinute:              120,
		MoveMaxPerMinutePerSourceHost: 60,
		MoveMaxPerMinutePerTargetHost: 40,
		MoveMaxPerTick:                10,
		MoveBalanceTolerance:          0.10,
		MoveSourceMinCpu:              0,
		MoveTargetMaxCpu:              0.75,
		MoveEmergencyCpu:              0.95,
		MoveEmergencyTicks:            3,
		MoveIdleTimeout:               10 * time.Minute,
		MoveMinResidentAge:            10 * time.Minute,
		MoveClientCooldown:            6 * time.Hour,
		EnableArrivalMoves:            false,
		MoveDbWaitPauseThreshold:      50 * time.Millisecond,
		MoveInventoryRefreshInterval:  5 * time.Minute,
		MoveTargetBreakerRatio:        0.2,
		MoveTargetBreakerMinSends:     20,
		MoveTargetBreakerTimeout:      10 * time.Minute,
		MoveTargetBreakerWindow:       10 * time.Minute,
	}
}
