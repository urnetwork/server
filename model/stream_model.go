package model

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	// "encoding/json"
	"bytes"
	"encoding/gob"
	"fmt"
	"iter"
	"sync"
	"time"

	"golang.org/x/exp/maps"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// note `streamKey` and `streamHop` are []byte so they can be directly stored into redis

type streamKey []byte

// stream keys are stored so that source id < destination id
// in this way the sender and receiver paths use the same stream
func newStreamKey(sourceId server.Id, destinationId server.Id, intermediaryIds []server.Id) streamKey {
	c := sourceId.Cmp(destinationId)
	for i := 0; c == 0 && i < len(intermediaryIds)-1-i; i += 1 {
		c = intermediaryIds[i].Cmp(intermediaryIds[len(intermediaryIds)-1-i])
	}

	sk := make([]byte, 16*(2+len(intermediaryIds)))
	if c <= 0 {
		// order source, intermed..., dest
		copy(sk, sourceId[:])
		for i := 0; i < len(intermediaryIds); i += 1 {
			intermediaryId := intermediaryIds[i]
			copy(sk[(i+1)*16:], intermediaryId[:])
		}
		copy(sk[(len(intermediaryIds)+1)*16:], destinationId[:])
	} else {
		// order dest, reverse(intermed...), source
		copy(sk, destinationId[:])
		for i := 0; i < len(intermediaryIds); i += 1 {
			intermediaryId := intermediaryIds[len(intermediaryIds)-1-i]
			copy(sk[(i+1)*16:], intermediaryId[:])
		}
		copy(sk[(len(intermediaryIds)+1)*16:], sourceId[:])
	}
	return sk
}

func (self streamKey) Bytes() []byte {
	return []byte(self)
}

func (self streamKey) SourceId() server.Id {
	return server.Id(self[:16])
}

func (self streamKey) DestinationId() server.Id {
	return server.Id(self[len(self)-16 : len(self)])
}

func (self streamKey) IntermediaryIds() (intermediaryIds []server.Id) {
	n := (len(self) / 16) - 2
	for i := range n {
		intermediaryId := server.Id(self[(i+1)*16 : (i+2)*16])
		intermediaryIds = append(intermediaryIds, intermediaryId)
	}
	return
}

// a fixed length string representation of the stream key
func (self streamKey) String() string {
	h := sha256.New()
	d := h.Sum(self)
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)
	return encoder.EncodeToString(d)
}

// enumerate ids and optional source and destination for each id
func (self streamKey) Edges() iter.Seq2[server.Id, [2]*server.Id] {
	return func(yield func(server.Id, [2]*server.Id) bool) {
		n := len(self) / 16
		for i := range n {
			clientId := server.Id(self[i*16 : (i+1)*16])
			var edges [2]*server.Id
			if 0 <= i-1 {
				sourceId := server.Id(self[(i-1)*16 : i*16])
				edges[0] = &sourceId
			}
			if i+1 < n {
				destinationId := server.Id(self[(i+1)*16 : (i+2)*16])
				edges[1] = &destinationId
			}
			if !yield(clientId, edges) {
				return
			}
		}
	}
}

type StreamHop [48]byte

func NewStreamHop(sourceId server.Id, destinationId server.Id, streamId server.Id) StreamHop {
	var sh [48]byte
	copy(sh[:], sourceId[:])
	copy(sh[16:], destinationId[:])
	copy(sh[32:], streamId[:])
	return sh
}

func (self StreamHop) Bytes() []byte {
	return []byte(self[:])
}

func (self StreamHop) SourceId() server.Id {
	return server.Id(self[0:16])
}

func (self StreamHop) DestinationId() server.Id {
	return server.Id(self[16:32])
}

func (self StreamHop) StreamId() server.Id {
	return server.Id(self[32:48])
}

func (self StreamHop) Path() connect.TransferPath {
	sourceId := connect.Id(self[0:16])
	destinationId := connect.Id(self[16:32])
	streamId := connect.Id(self[32:48])
	return connect.TransferPath{
		SourceId:      sourceId,
		DestinationId: destinationId,
		StreamId:      streamId,
	}
}

// all keys have ttl of 1 day that is reset for each new contract
// meaning if there is no new activity, they will be cleared
// stream key -> stream id
// stream key -> set of contract id
// client id -> set of active stream pairs (from, to, streamid)
// client id counter
// event(client id) active streams changed
// contract id -> stream key

func streamIdKey(streamKey []byte) string {
	return fmt.Sprintf("{%s}s_sk_sid", streamKey)
}

func streamContractsKey(streamKey []byte) string {
	return fmt.Sprintf("{%s}s_sk_cs", streamKey)
}

func clientStreamHopsKey(clientId server.Id) string {
	return fmt.Sprintf("{%s}s_c_hops", clientId)
}

func clientEventIdKey(clientId server.Id) string {
	return fmt.Sprintf("{%s}s_c_eid", clientId)
}

func clientStreamHopEvents(clientId server.Id) string {
	return fmt.Sprintf("{%s}s_c_events", clientId)
}

func contractStreamKey(contractId server.Id) string {
	return fmt.Sprintf("{%s}s_ct_sk", contractId)
}

func contractStreamId(contractId server.Id) string {
	return fmt.Sprintf("{%s}s_ct_sid", contractId)
}

// returns a stream id to use for the contract
// if stream count changes from 0, associate the stream id with each hop on the stream
func AddToStream(
	ctx context.Context,
	contractId server.Id,
	sourceId server.Id,
	destinationId server.Id,
	intermediaryIds []server.Id,
) (
	streamId server.Id,
) {
	ttl := 8 * time.Hour
	streamKey := newStreamKey(sourceId, destinationId, intermediaryIds)

	// will be overwritten if already set
	server.Redis(ctx, func(r server.RedisClient) {
		// note the keys in eval have to be in the same hash slot
		result := r.Eval(
			ctx,
			`
			local stream_id_key = KEYS[1]
			local stream_contracts_key = KEYS[2]
			local stream_id = ARGV[1]
			local contract_id = ARGV[2]
			local ttl = ARGV[3]

			local initial_size = redis.call('SCARD', stream_contracts_key)
			local changed = redis.call('SADD', stream_contracts_key, contract_id)

			if initial_size == 0 then
				if 0 < changed then
			    	redis.call('SET', stream_id_key, stream_id)
			    end
			else
				stream_id = redis.call('GET', stream_id_key)
			end

			if 0 < changed then
				redis.call('EXPIRE', stream_id_key, ttl)
				redis.call('EXPIRE', stream_contracts_key, ttl)
			end

			return {stream_id, initial_size, changed}
			`,
			[]string{
				streamIdKey(streamKey),
				streamContractsKey(streamKey),
			},
			server.NewId().Bytes(),
			contractId.Bytes(),
			ttl,
		)
		values, err := result.Slice()
		if err != nil {
			panic(err)
		}

		streamId = server.Id([]byte(values[0].(string)))
		initialSize := values[1].(int64)
		changed := 0 < values[2].(int64)

		if changed {
			pipe := r.TxPipeline()
			pipe.Set(ctx, contractStreamKey(contractId), streamKey.Bytes(), ttl)
			pipe.Set(ctx, contractStreamId(contractId), streamId.Bytes(), ttl)
			_, err := pipe.Exec(ctx)
			if err != nil {
				panic(err)
			}

			if initialSize == 0 {
				for clientId, edges := range streamKey.Edges() {
					eventId, err := r.Incr(ctx, clientEventIdKey(clientId)).Result()
					if err != nil {
						panic(err)
					}

					streamHopsKey := clientStreamHopsKey(clientId)
					pipe := r.TxPipeline()

					event := &StreamHopEvent{
						StreamHopEventType: StreamHopEventTypeAdded,
						EventId:            eventId,
					}

					if edges[0] != nil {
						streamHop := NewStreamHop(*edges[0], clientId, streamId)
						pipe.SAdd(ctx, streamHopsKey, streamHop.Bytes())
						event.StreamHops = append(event.StreamHops, streamHop)
					}
					if edges[1] != nil {
						streamHop := NewStreamHop(clientId, *edges[1], streamId)
						pipe.SAdd(ctx, streamHopsKey, streamHop.Bytes())
						event.StreamHops = append(event.StreamHops, streamHop)
					}

					buf := bytes.NewBuffer(nil)
					encoder := gob.NewEncoder(buf)
					err = encoder.Encode(event)
					if err != nil {
						panic(err)
					}
					eventBytes := buf.Bytes()
					pipe.SPublish(ctx, clientStreamHopEvents(clientId), eventBytes)

					pipe.Expire(ctx, streamHopsKey, ttl)
					_, err = pipe.Exec(ctx)
					if err != nil {
						panic(err)
					}
				}
			} else {
				for clientId, _ := range streamKey.Edges() {
					streamHopsKey := clientStreamHopsKey(clientId)
					r.Expire(ctx, streamHopsKey, ttl)
				}
			}
		}
	})
	return
}

func RemoveFromStream(ctx context.Context, contractId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		streamKeyCmd := pipe.Get(ctx, contractStreamKey(contractId))
		streamIdCmd := pipe.Get(ctx, contractStreamId(contractId))
		_, err := pipe.Exec(ctx)
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}

		streamKeyBytes, err := streamKeyCmd.Bytes()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamKey := streamKey(streamKeyBytes)

		streamIdBytes, err := streamIdCmd.Bytes()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamId := server.Id(streamIdBytes)

		// note the keys in eval have to be in the same hash slot
		result := r.Eval(
			ctx,
			`
			local stream_id_key = KEYS[1]
			local stream_contracts_key = KEYS[2]
			local contract_id = ARGV[1]

			local changed = redis.call('SREM', stream_contracts_key, contract_id)
			local final_size = redis.call('SCARD', stream_contracts_key)

			if final_size == 0 then
				if 0 < changed then
			    	redis.call('DEL', stream_id_key)
			    end
			end

			return {final_size, changed}
			`,
			[]string{
				streamIdKey(streamKey),
				streamContractsKey(streamKey),
			},
			contractId.Bytes(),
		)
		values, err := result.Slice()
		if err != nil {
			panic(err)
		}

		finalSize := values[0].(int64)
		changed := 0 < values[1].(int64)

		if changed {
			pipe := r.TxPipeline()
			pipe.Del(ctx, contractStreamKey(contractId))
			pipe.Del(ctx, contractStreamId(contractId))
			_, err := pipe.Exec(ctx)
			if err != nil {
				panic(err)
			}

			if finalSize == 0 {
				for clientId, edges := range streamKey.Edges() {
					eventId, err := r.Incr(ctx, clientEventIdKey(clientId)).Result()
					if err != nil {
						panic(err)
					}

					streamHopsKey := clientStreamHopsKey(clientId)
					pipe := r.TxPipeline()

					event := &StreamHopEvent{
						StreamHopEventType: StreamHopEventTypeRemoved,
						EventId:            eventId,
					}

					if edges[0] != nil {
						streamHop := NewStreamHop(*edges[0], clientId, streamId)
						pipe.SRem(ctx, streamHopsKey, streamHop.Bytes())
						event.StreamHops = append(event.StreamHops, streamHop)
					}
					if edges[1] != nil {
						streamHop := NewStreamHop(clientId, *edges[1], streamId)
						pipe.SRem(ctx, streamHopsKey, streamHop.Bytes())
						event.StreamHops = append(event.StreamHops, streamHop)
					}

					buf := bytes.NewBuffer(nil)
					encoder := gob.NewEncoder(buf)
					err = encoder.Encode(event)
					if err != nil {
						panic(err)
					}
					eventBytes := buf.Bytes()
					pipe.SPublish(ctx, clientStreamHopEvents(clientId), eventBytes)

					_, err = pipe.Exec(ctx)
					if err != nil {
						panic(err)
					}
				}
			}
		}
	})
	return
}

func GetStream(ctx context.Context, contractId server.Id) (streamId server.Id, key streamKey, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		streamIdCmd := r.Get(ctx, contractStreamId(contractId))
		streamKeyCmd := r.Get(ctx, contractStreamKey(contractId))
		_, err := pipe.Exec(ctx)
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}

		streamIdBytes, err := streamIdCmd.Bytes()

		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamId = server.Id(streamIdBytes)

		streamKeyBytes, err := streamKeyCmd.Bytes()

		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		key = streamKey(streamKeyBytes)

		ok = true
	})
	return
}

func GetStreamEventId(ctx context.Context, clientId server.Id) (eventId int64) {
	server.Redis(ctx, func(r server.RedisClient) {
		eventId_, err := r.Get(ctx, clientEventIdKey(clientId)).Int64()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		eventId = eventId_
	})
	return
}

func GetStreamHops(ctx context.Context, clientId server.Id) (eventId int64, streamHops map[StreamHop]bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		streamHopsCmd := pipe.SMembers(ctx, clientStreamHopsKey(clientId))
		eventIdCmd := pipe.Get(ctx, clientEventIdKey(clientId))
		_, err := pipe.Exec(ctx)
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}

		streamHopStrs, err := streamHopsCmd.Result()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		eventId_, err := eventIdCmd.Int64()
		if err == server.RedisNil {
			return
		}
		if err != nil {
			panic(err)
		}
		eventId = eventId_

		streamHops = map[StreamHop]bool{}
		for _, streamHopStr := range streamHopStrs {
			streamHop := StreamHop([]byte(streamHopStr))
			streamHops[streamHop] = true
		}
	})
	return
}

type StreamHopEventType int

const (
	StreamHopEventTypeAdded   StreamHopEventType = 1
	StreamHopEventTypeRemoved StreamHopEventType = 2
	StreamHopEventTypeReset   StreamHopEventType = 3
)

type StreamHopEvent struct {
	EventId int64
	// add, remove, reset
	StreamHopEventType StreamHopEventType
	StreamHops         []StreamHop
}

type StreamHopListener struct {
	ctx         context.Context
	cancel      context.CancelFunc
	clientId    server.Id
	callback    func(*StreamHopEvent)
	pollTimeout time.Duration
}

func NewStreamHopListener(ctx context.Context, clientId server.Id, callback func(*StreamHopEvent), pollTimeout time.Duration) *StreamHopListener {
	cancelCtx, cancel := context.WithCancel(ctx)

	shl := &StreamHopListener{
		ctx:         cancelCtx,
		cancel:      cancel,
		clientId:    clientId,
		callback:    callback,
		pollTimeout: pollTimeout,
	}
	go server.HandleError(shl.run)
	return shl
}

func (self *StreamHopListener) run() {
	defer self.cancel()

	messages, unsub := server.Subscribe(self.ctx, clientStreamHopEvents(self.clientId))
	defer unsub()

	// the last processed event id
	// event ids start at 1
	var eventId int64

	reset := func() {
		if resetEventId, resetStreamHops := GetStreamHops(self.ctx, self.clientId); eventId < resetEventId {
			eventId = resetEventId

			resetEvent := &StreamHopEvent{
				StreamHopEventType: StreamHopEventTypeReset,
				EventId:            resetEventId,
				StreamHops:         maps.Keys(resetStreamHops),
			}
			self.callback(resetEvent)
		}
	}

	for {
		select {
		case <-self.ctx.Done():
			return
		case m := <-messages:
			switch v := m.(type) {
			case server.RedisSubscription:
				switch v.Kind {
				case "subscribe", "ssubscribe":
					reset()
				}
			case server.RedisMessage:
				buf := bytes.NewBuffer([]byte(v.Payload))
				decoder := gob.NewDecoder(buf)
				var event StreamHopEvent
				err := decoder.Decode(&event)
				if err == nil {
					if eventId < event.EventId {
						if eventId+1 == event.EventId {
							eventId = event.EventId
							self.callback(&event)
						} else {
							// a gap in delivery, reset
							reset()
						}
					}
				}
			}
		case <-time.After(self.pollTimeout):
			// no event, poll in case events are not being delivered
			if eventId < GetStreamEventId(self.ctx, self.clientId) {
				reset()
			}
		}
	}
}

func (self *StreamHopListener) Close() {
	self.cancel()
}

type StreamHopAccumulator struct {
	addedCallback   func(StreamHop)
	removedCallback func(StreamHop)

	stateLock sync.Mutex
	hops      map[StreamHop]bool
}

func NewStreamHopAccumulator(addedCallback func(StreamHop), removedCallback func(StreamHop)) *StreamHopAccumulator {
	return &StreamHopAccumulator{
		addedCallback:   addedCallback,
		removedCallback: removedCallback,
		hops:            map[StreamHop]bool{},
	}
}

func (self *StreamHopAccumulator) Event(event *StreamHopEvent) {
	var added []StreamHop
	var removed []StreamHop

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		switch event.StreamHopEventType {
		case StreamHopEventTypeAdded:
			for _, hop := range event.StreamHops {
				if !self.hops[hop] {
					self.hops[hop] = true
					added = append(added, hop)
				}
			}
		case StreamHopEventTypeRemoved:
			for _, hop := range event.StreamHops {
				if self.hops[hop] {
					delete(self.hops, hop)
					removed = append(removed, hop)
				}
			}
		case StreamHopEventTypeReset:
			hops := map[StreamHop]bool{}
			for _, hop := range event.StreamHops {
				hops[hop] = true
			}

			for hop, _ := range self.hops {
				if !hops[hop] {
					removed = append(removed, hop)
				}
			}
			for hop, _ := range hops {
				if !self.hops[hop] {
					added = append(added, hop)
				}
			}
			self.hops = hops
		}
	}()

	if self.addedCallback != nil {
		for _, hop := range added {
			self.addedCallback(hop)
		}
	}
	if self.removedCallback != nil {
		for _, hop := range removed {
			self.removedCallback(hop)
		}
	}
}

func (self *StreamHopAccumulator) StreamHops() map[StreamHop]bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	streamHops := maps.Clone(self.hops)
	return streamHops
}

func (self *StreamHopAccumulator) StreamIds() map[server.Id]bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	streamIds := map[server.Id]bool{}
	for hop, _ := range self.hops {
		streamIds[hop.StreamId()] = true
	}
	return streamIds
}
