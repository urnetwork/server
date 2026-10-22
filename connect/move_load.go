// The load publisher and the edge inventory of the mover (connect/MOVE.md
// §5.4, D3, D17).
//
// Every connect process publishes its load record into the Redis load table
// (model/connect_move_load.go) every MoveLoadPublishInterval, from the moment
// it starts until it stops, in every mover mode: the table feeds the load
// gauges before anything moves. A record holds the process's resident,
// provider resident and transport counts, whether it drains, the cores it used
// and the host's CPU busy fraction over the publisher's last interval, and the
// host's core count. Host CPU comes from /proc/stat, which is host-wide inside
// the container; cores from runtime.NumCPU(). A process whose publisher stops
// removes its record, and a record its process never removed goes stale and is
// ignored, then deleted by the movers.
//
// The inventory is the vault `services.yml` lb interfaces, which the move
// primitives parse. The mover's cache reloads it every
// MoveInventoryRefreshInterval and keeps the last good inventory when a load
// fails, so edge hosts added in a new inventory version start receiving moves
// without a deploy.
package connect

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A process's field in the load table: `<host>/<block>/<exchangeId>`.
type MoveProcessIdentity struct {
	// WARP_HOST, as the inventory names the host
	Host string
	// WARP_BLOCK
	Block string
	// unique to the process, so a replacement container never shares a field
	ExchangeId server.Id
}

// The process's load as its record carries it.
type MoveLoadCounts struct {
	// distinct residents
	Residents int
	// residents whose client provides
	ProviderResidents int
	// client transports
	Transports int
	// the process is draining
	Draining bool
}

// The process's load. Implemented by the exchange.
type MoveLoadSource interface {
	// The current counts. An in-memory read that does not block.
	MoveLoadCounts() MoveLoadCounts
}

// One public interface of an edge host.
type MoveInventoryInterface struct {
	// the interface's key in the inventory
	Name string
	// the public IPv4 address; invalid when it has none
	Ipv4 netip.Addr
	// the public IPv6 address; invalid when it has none
	Ipv6 netip.Addr
	// its route53 weight (`dns_weight`). Non-positive: never a target.
	DnsWeight int
	// a transparent interface is never a target
	Transparent bool
}

// One edge host of the inventory.
type MoveInventoryHost struct {
	Interfaces []MoveInventoryInterface
	// The optional `move_weight`: capacity is cores times this. Non-positive
	// means 1.
	MoveWeight float64
}

// The edge inventory. Implemented by the move primitives' inventory parser.
type MoveInventoryLoader interface {
	// The current `versions[0].lb.interfaces` of the vault `services.yml`
	// (D17), by host. The host names are the ones processes publish as their
	// host (WARP_HOST), so the inventory joins the load table. Called from the
	// mover's goroutine within ctx's deadline at most every
	// MoveInventoryRefreshInterval, and again the next tick after an error.
	LoadMoveInventory(ctx context.Context) (map[string]MoveInventoryHost, error)
}

// The mover's copy of the inventory: reloaded every
// MoveInventoryRefreshInterval, the last good one kept while a load fails. Not
// safe for concurrent use.
type moveInventoryCache struct {
	loader   MoveInventoryLoader
	settings *ExchangeMoverSettings

	inventory map[string]MoveInventoryHost
	loaded    bool
	// zero until the first load succeeds: load at every call until then
	nextLoadTime time.Time
}

// The inventory at now, reloading it when its refresh is due. After a failed
// load the last good inventory is kept and the next call loads again; with
// none yet, the inventory is empty and nothing is a target.
func (self *moveInventoryCache) hosts(ctx context.Context, now time.Time) map[string]MoveInventoryHost {
	refreshInterval := self.settings.MoveInventoryRefreshInterval
	if self.loaded && (refreshInterval <= 0 || now.Before(self.nextLoadTime)) {
		return self.inventory
	}
	inventory, err := self.loader.LoadMoveInventory(ctx)
	if err != nil {
		if glog.V(1) {
			glog.Infof("[move]inventory load failed; keeping the last (%s)\n", err)
		}
		return self.inventory
	}
	self.inventory = inventory
	self.loaded = true
	self.nextLoadTime = now.Add(refreshInterval)
	return self.inventory
}

// One measurement of the publisher's interval.
type moveCpuSample struct {
	// cores the process used
	processCpu float64
	// busy fraction of all host CPUs, or model.ConnectMoveLoadCpuUnknown
	hostCpuBusy float64
	hostCores   int
}

// Measures CPU over the time since its previous measurement.
type moveCpuSampler interface {
	// The measurement since the previous call. The first call has no
	// interval: the process CPU is zero and the host CPU unknown.
	NextCpuSample() moveCpuSample
}

// The aggregate CPU time of /proc/stat, in clock ticks.
type procStatCpu struct {
	// everything except idle and iowait
	busy  uint64
	total uint64
}

// The aggregate `cpu` line of /proc/stat: user, nice, system, idle, iowait,
// irq, softirq, steal, guest and guest_nice, of which kernels before 2.6.33
// report fewer. Guest time is already in user and nice, so it is not added
// again. Busy is the total less idle and iowait.
func parseProcStatCpu(reader io.Reader) (procStatCpu, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		values := fields[1:]
		if len(values) < 4 {
			return procStatCpu{}, fmt.Errorf("/proc/stat cpu line has %d values", len(values))
		}
		// user nice system idle iowait irq softirq steal
		values = values[:min(len(values), 8)]
		var cpu procStatCpu
		idle := uint64(0)
		for i, value := range values {
			ticks, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return procStatCpu{}, err
			}
			cpu.total += ticks
			if i == 3 || i == 4 {
				idle += ticks
			}
		}
		cpu.busy = cpu.total - idle
		return cpu, nil
	}
	if err := scanner.Err(); err != nil {
		return procStatCpu{}, err
	}
	return procStatCpu{}, errors.New("/proc/stat has no cpu line")
}

// The busy fraction between previous and this, or
// model.ConnectMoveLoadCpuUnknown when no time passed or a counter went
// backwards.
func (self procStatCpu) busyFractionSince(previous procStatCpu) float64 {
	if self.total <= previous.total || self.busy < previous.busy {
		return model.ConnectMoveLoadCpuUnknown
	}
	busy := float64(self.busy-previous.busy) / float64(self.total-previous.total)
	return min(1, max(0, busy))
}

// Measures the host from a /proc/stat file and the process from its rusage.
type procMoveCpuSampler struct {
	procStatPath string
	// the process's user and system CPU time, false where it cannot be read
	processCpuTime func() (time.Duration, bool)
	now            func() time.Time
	hostCores      int

	previousHost      procStatCpu
	previousHostOk    bool
	previousProcess   time.Duration
	previousProcessOk bool
	previousTime      time.Time
}

func newProcMoveCpuSampler() *procMoveCpuSampler {
	return &procMoveCpuSampler{
		procStatPath:   "/proc/stat",
		processCpuTime: moveProcessCpuTime,
		now:            time.Now,
		hostCores:      runtime.NumCPU(),
	}
}

func (self *procMoveCpuSampler) NextCpuSample() moveCpuSample {
	now := self.now()
	sample := moveCpuSample{
		processCpu:  0,
		hostCpuBusy: model.ConnectMoveLoadCpuUnknown,
		hostCores:   self.hostCores,
	}

	host, hostErr := func() (procStatCpu, error) {
		file, err := os.Open(self.procStatPath)
		if err != nil {
			return procStatCpu{}, err
		}
		defer file.Close()
		return parseProcStatCpu(file)
	}()
	if hostErr == nil && self.previousHostOk {
		sample.hostCpuBusy = host.busyFractionSince(self.previousHost)
	}

	process, processOk := self.processCpuTime()
	elapsed := now.Sub(self.previousTime)
	if processOk && self.previousProcessOk && 0 < elapsed && self.previousProcess <= process {
		sample.processCpu = (process - self.previousProcess).Seconds() / elapsed.Seconds()
	}

	self.previousHost, self.previousHostOk = host, hostErr == nil
	self.previousProcess, self.previousProcessOk = process, processOk
	self.previousTime = now
	return sample
}

// Where the publisher writes its record. Tests replace it.
type moveLoadStore interface {
	publishLoad(ctx context.Context, load *model.ConnectMoveLoad) (time.Time, error)
	removeLoad(ctx context.Context, identity MoveProcessIdentity) error
}

func (moveModelStore) publishLoad(ctx context.Context, load *model.ConnectMoveLoad) (time.Time, error) {
	return model.PublishConnectMoveLoad(ctx, load)
}

func (moveModelStore) removeLoad(ctx context.Context, identity MoveProcessIdentity) error {
	return model.RemoveConnectMoveLoad(ctx, identity.Host, identity.Block, identity.ExchangeId)
}

// Publishes one process's load record. See the file header. Not safe for
// concurrent use except Close: one loop owns the sampler.
type MoveLoadPublisher struct {
	ctx    context.Context
	cancel context.CancelFunc

	identity MoveProcessIdentity
	settings *ExchangeSettings
	source   MoveLoadSource
	sampler  moveCpuSampler
	store    moveLoadStore
}

// Starts publishing the process's record until ctx ends or Close, then
// removes it. With EnableMove off, or a non-positive MoveLoadPublishInterval,
// it publishes nothing.
func NewMoveLoadPublisher(
	ctx context.Context,
	identity MoveProcessIdentity,
	settings *ExchangeSettings,
	source MoveLoadSource,
) *MoveLoadPublisher {
	return startMoveLoadPublisher(newMoveLoadPublisherWithStore(ctx, identity, settings, source, newProcMoveCpuSampler(), moveModelStore{}))
}

// Starts the publisher's loop.
func startMoveLoadPublisher(publisher *MoveLoadPublisher) *MoveLoadPublisher {
	go server.HandleError(publisher.run)
	return publisher
}

// The publisher over an explicit sampler and store, without its loop. Tests
// drive its publishes directly.
func newMoveLoadPublisherWithStore(
	ctx context.Context,
	identity MoveProcessIdentity,
	settings *ExchangeSettings,
	source MoveLoadSource,
	sampler moveCpuSampler,
	store moveLoadStore,
) *MoveLoadPublisher {
	cancelCtx, cancel := context.WithCancel(ctx)
	return &MoveLoadPublisher{
		ctx:      cancelCtx,
		cancel:   cancel,
		identity: identity,
		settings: settings,
		source:   source,
		sampler:  sampler,
		store:    store,
	}
}

// Publishes at once, then every interval, until the publisher closes; then
// removes the record.
func (self *MoveLoadPublisher) run() {
	defer self.cancel()
	interval := self.settings.MoveLoadPublishInterval
	if !self.settings.EnableMove || interval <= 0 {
		return
	}
	defer self.remove()
	for {
		self.publish()
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Stops publishing. The record is removed as the loop ends.
func (self *MoveLoadPublisher) Close() {
	self.cancel()
}

// Writes the record once, within one publish interval. A failure is counted;
// the next publish repairs it.
func (self *MoveLoadPublisher) publish() error {
	publishCtx, publishCancel := context.WithTimeout(self.ctx, self.settings.MoveLoadPublishInterval)
	defer publishCancel()

	counts := self.source.MoveLoadCounts()
	sample := self.sampler.NextCpuSample()
	if 0 <= sample.hostCpuBusy && !math.IsNaN(sample.hostCpuBusy) {
		moveHostCpuBusyGauge.WithLabelValues().Set(sample.hostCpuBusy)
	} else {
		moveHostCpuBusyGauge.Reset()
	}
	_, err := self.store.publishLoad(publishCtx, &model.ConnectMoveLoad{
		Host:              self.identity.Host,
		Block:             self.identity.Block,
		ExchangeId:        self.identity.ExchangeId,
		Residents:         counts.Residents,
		ProviderResidents: counts.ProviderResidents,
		Transports:        counts.Transports,
		ProcessCpu:        sample.processCpu,
		HostCpuBusy:       sample.hostCpuBusy,
		HostCores:         sample.hostCores,
		Draining:          counts.Draining,
	})
	if err != nil {
		moveLoadPublishesCounter.WithLabelValues(moveLoadPublishFailed).Inc()
		if glog.V(1) {
			glog.Infof("[move]load publish failed (%s)\n", err)
		}
		return err
	}
	moveLoadPublishesCounter.WithLabelValues(moveLoadPublished).Inc()
	return nil
}

// Removes the record, within one publish interval of a context that outlives
// the publisher's.
func (self *MoveLoadPublisher) remove() error {
	removeCtx, removeCancel := context.WithTimeout(context.Background(), self.settings.MoveLoadPublishInterval)
	defer removeCancel()
	err := self.store.removeLoad(removeCtx, self.identity)
	if err != nil && glog.V(1) {
		glog.Infof("[move]load remove failed (%s)\n", err)
	}
	return err
}
