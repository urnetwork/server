// A refused reservation costs a preflight PING and an EVAL of the 5.8 KB
// reservation script with up to five replicated writes, and its grant joins
// this request's recovery page. A payer whose grants are all reserved repeats
// that for every grant on every request, only to prove that nothing is left.
// After a request's first refusal, pipelined read-only probes of its next
// candidates read what each grant's reservation would find. Selection skips a
// reservation only when its probe proves the refusal; any other state defers
// to the reservation itself.
package model

import (
	"context"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// The probe proves a refusal only for the reservation script's plain state: a
// valid counter generation with no lease expired and no token of this request.
// There the script's availability is credit - legacy - total, so a partial
// reservation refuses when it is zero and a whole one when it is below the
// request. A missing or reset generation, an expired lease, this request's own
// token or an invalid counter returns an empty availability, and an invalid
// key type fails the probe: each defers to the reservation. Every key the
// reservation type-checks is read with a command that fails on the wrong type.
// The probe also reports whether the grant holds no recovery or pending
// marker, where this request's recovery page would find no work. It writes
// nothing.
const redisGrantProbeBody = `
local total=redis.call('GET',KEYS[1])
local legacy=redis.call('GET',KEYS[4]) or '0'
if not total or not valid(total) or not valid(legacy) or not valid(ARGV[2]) then return {'',0} end
if total~='0' and (redis.call('EXISTS',KEYS[2])==0 or redis.call('EXISTS',KEYS[3])==0) then return {'',0} end
local now=redis.call('TIME'); local milliseconds=tonumber(now[1])*1000+math.floor(tonumber(now[2])/1000)
if #redis.call('ZRANGEBYSCORE',KEYS[3],'-inf',milliseconds,'LIMIT',0,1)~=0 then return {'',0} end
if redis.call('HEXISTS',KEYS[2],ARGV[1])==1 then return {'',0} end
local recovery=redis.call('ZCARD',KEYS[5]); local pending=redis.call('ZCARD',KEYS[6])
local idle=0; if recovery==0 and pending==0 then idle=1 end
return {sub(sub(ARGV[2],legacy),total),idle}
`

// The reservation script's decimal validation and arithmetic, verbatim, so
// the probe and the reservation compute one availability from one state.
var redisGrantProbeScript = func() string {
	prelude, _, found := strings.Cut(redisContractReservationScript, "local rawTotal=")
	if !found || !strings.Contains(prelude, "local function sub(a,b)") {
		panic("Redis reservation script helpers moved; the grant probe must share them")
	}
	return prelude + strings.TrimPrefix(redisGrantProbeBody, "\n")
}()

// One grant's probe result. Only an exact result can prove a refusal.
type redisGrantProbeVerdict struct {
	exact     bool
	available ByteCount
	// no recovery or pending marker: the request's recovery page finds no work
	idle bool
}

// Whether the reservation the probe observed would reserve nothing: a whole
// reservation needs every remaining byte, a partial one any byte.
func (self redisGrantProbeVerdict) refuses(remaining ByteCount, whole bool) bool {
	if !self.exact {
		return false
	}
	if whole {
		return self.available < remaining
	}
	return self.available == 0
}

// Reads one probe reply. Anything but an exact availability defers to the
// reservation; the result is the finite metric outcome.
func parseRedisGrantProbe(values []any) (verdict redisGrantProbeVerdict, result string) {
	if len(values) != 2 {
		return verdict, "error"
	}
	text, ok := values[0].(string)
	idle, idleOk := values[1].(int64)
	if !ok || !idleOk || (idle != 0 && idle != 1) {
		return verdict, "error"
	}
	if text == "" {
		return verdict, "undecided"
	}
	available, err := strconv.ParseInt(text, 10, 64)
	if err != nil || available < 0 || strconv.FormatInt(available, 10) != text {
		return verdict, "error"
	}
	verdict = redisGrantProbeVerdict{exact: true, available: available, idle: idle == 1}
	if available == 0 {
		return verdict, "empty"
	}
	return verdict, "available"
}

// One selection's probe observations. Probing starts after the request's
// first refused reservation, so a request funded by its first attempt reads
// nothing more. Each probe pipeline reads the next candidates, one grant at
// first and twice as many each time after, so a request funded soon after its
// refusal reads few grants and one with every grant reserved needs a
// logarithmic number of round trips. A batch is a spread window's attempts or
// one keyset page; its observations are read immediately before its own
// attempts and are forgotten before the next batch.
type redisGrantProbe struct {
	contractId server.Id
	active     bool
	// grants the next probe pipeline reads
	width    int
	verdicts map[server.Id]redisGrantProbeVerdict
}

func (self *redisGrantProbe) noteRefusal() {
	if !self.active {
		self.active = true
		self.width = 1
	}
}

// Starts a batch. No observation outlives the batch it was read for.
func (self *redisGrantProbe) forget() {
	self.verdicts = nil
}

// Chooses the grants the next probe pipeline reads, starting at the next
// attempt, once the request has a refusal, and records each as observed with
// an inexact verdict until its read returns. A grant is chosen at most once
// per batch.
func (self *redisGrantProbe) take(upcoming []*escrowTransferBalance) []*escrowTransferBalance {
	if !self.active || len(upcoming) == 0 {
		return nil
	}
	if _, observed := self.verdicts[upcoming[0].balanceId]; observed {
		return nil
	}
	if self.verdicts == nil {
		self.verdicts = map[server.Id]redisGrantProbeVerdict{}
	}
	probed := make([]*escrowTransferBalance, 0, min(self.width, len(upcoming)))
	for _, balance := range upcoming {
		if len(probed) == self.width {
			break
		}
		if _, observed := self.verdicts[balance.balanceId]; observed {
			continue
		}
		self.verdicts[balance.balanceId] = redisGrantProbeVerdict{}
		// selection never reserves a grant without durable credit
		if 0 < balance.balanceByteCount {
			probed = append(probed, balance)
		}
	}
	if len(probed) != 0 {
		self.width = min(2*self.width, redisGrantPageRows)
	}
	return probed
}

// Reads the grants take chooses in one pipeline. A failed read leaves an
// inexact verdict and its reservation decides.
func (self *redisGrantProbe) observe(ctx context.Context, upcoming []*escrowTransferBalance) {
	probed := self.take(upcoming)
	if len(probed) == 0 {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, redisContractAdmissionTimeout)
	defer cancel()
	commands := make([]*redis.Cmd, len(probed))
	// Read-only and judged one by one: no preflight, and a failed read is
	// only an inexact verdict.
	server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
		_, err := client.Pipelined(bounded, func(pipe redis.Pipeliner) error {
			for index, balance := range probed {
				commands[index] = pipe.Eval(bounded, redisGrantProbeScript, redisContractReservationKeys(balance.balanceId),
					self.contractId.String(), strconv.FormatInt(balance.balanceByteCount, 10))
			}
			return nil
		})
		return err
	}, server.OptRedisNoPreflight())
	for index, balance := range probed {
		result := "error"
		if command := commands[index]; command != nil {
			if values, err := command.Slice(); err == nil {
				self.verdicts[balance.balanceId], result = parseRedisGrantProbe(values)
			}
		}
		redisContractReservationResults.WithLabelValues("reserve-probe", result).Inc()
	}
}

// Whether this batch's probe proved that the reservation would refuse, and
// whether the grant's recovery page would find no work.
func (self *redisGrantProbe) refuses(balanceId server.Id, remaining ByteCount, whole bool) (refused bool, idle bool) {
	verdict := self.verdicts[balanceId]
	return verdict.refuses(remaining, whole), verdict.idle
}
