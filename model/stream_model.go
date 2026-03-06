package model



// note these types are []byte so they can be directly stored into redis

type streamKey []byte

// stream keys are stored so that source id < destination id
// in this way the sender and receiver paths use the same stream
func newStreamKey(sourceId server.Id, destinationId server.Id, intermediaryIds []server.Id) streamKey {
	c := sourceId.Cmp(destinationId)
	for i := 0; c == 0 && i < len(intermediaryIds) - 1 - i; i += 1 {
		c = intermediaryIds[i].Cmp(intermediaryIds[len(intermediaryIds) - 1 - i])
	}

	sk := make([]byte, 16 * (2 + len(intermediaryIds)))
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
			intermediaryId := intermediaryIds[len(intermediaryIds) - 1 - i]
			copy(sk[(i+1)*16:], intermediaryId[:])
		}
		copy(sk[(len(intermediaryIds)+1)*16:], sourceId[:])
	}
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
			clientId := server.Id(self[i*16:(i+1)*16])
			var edges [2]*server.Id
			if 0 <= i - 1 {
				sourceId := server.Id(self[(i-1)*16:i*16])
				edges[0] = &sourceId
			}
			if i + 1 < n {
				destinationId := server.Id(self[(i+1)*16:(i+2)*16])
				edges[1] = &destinationId
			}
			if !yield(clientId, edges) {
				return
			}
		}
	}
}



type streamHop []byte

func newStreamHop(sourceId server.Id, destinationId server.Id, streamId server.Id) []byte {
	sh := make([]byte, 48)
	copy(sh, sourceId[:])
	copy(sh[16:], destinationId[:])
	copy(sh[32:], streamId[:])
}

func (self streamHop) Path() TransferPath {
	sourceId := server.Id(self[0:16])
	destinationId := server.Id(self[16:32])
	streamId := server.Id(self[32:48])
	return TransferPath{
		SourceId: sourceId,
		DestinationId: destinationId,
		StreamId: streamId,
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
	return fmt.Sprintf("{%s}s_c_hops", clientId)
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

	streamKey := newStreamKey(sourceId, destinationId, intermediaryIds)

	
	// will be overwritten if already set
	streamId = server.NewId()
	server.Redis(ctx, func(r server.RedisClient) {
		// note the keys in eval have to be in the same hash slot
		result, err := r.Eval(
			```
			local stream_id_key = KEYS[1]
			local stream_contracts_key = KEYS[2]
			local stream_id = ARGV[1]
			local contract_id = ARGV[2]
			local ttl = ARGV[3]

			local initial_size = redis.call('SCARD', stream_contracts_key)
			local changed = redis.call('SADD', stream_contracts_key, contract_id)

			if initial_size == 0 then
				if changed then
			    	redis.call('SET', stream_id_key, stream_id)
			    end
			else
				stream_id = redis.call('GET', stream_id_key)
			end

			if changed then
				redis.call('EXPIRE', stream_id_key, ttl)
				redis.call('EXPIRE', stream_contracts_key, ttl)
			end

			return {stream_id, initial_size, changed}
			```,
			[]string{
				streamIdKey(streamKey),
				streamContractsKey(streamKey),
			},
			streamId.Bytes(),
			contractId.Bytes(),
			ttl,
		)
		if err != nil {
			panic(err)
		}
		values := result.Slice()

		streamId = server.Id(values[0])
		initialSize := int(values[1])
		changed := bool(values[2])

		if changed {
			pipe := r.TxPipeline()
			pipe.Set(contractStreamKey(contractId), streamKey, ttl)
			pipe.Set(contractStreamId(contractId), streamId, ttl)
			pipe.Exec()

			for clientId, edges := range streamKey.Edges() {
				streamHopsKey := clientStreamHopsKey(clientId)
				pipe := r.TxPipeline()
				if initialSize == 0 {
					eventId, err := r.Incr(clientEventIdKey(clientId)).Int64()
					if err != nil {
						panic(err)
					}
					event := &StreamHopEvent{
						StreamHopEventType: StreamHopEventTypeAdded,
						EventId: eventId,
					}

					if edges[0] != nil {
						streamHop := newStreamHop(*edges[0], clientId, streamId)
						pipe.Sadd(streamHopsKey, streamHop)
						event.StreamHops = append(event.StreamHops, streamHop)
					}
					if edges[1] != nil {
						streamHop := newStreamHop(clientId, *edges[1], streamId)
						pipe.Sadd(streamHopsKey, streamHop)
						event.StreamHops = append(event.StreamHops, streamHop)
					}

					eventBytes, err := gob.Marshal(event)
					if err != nil {
						panic(err)
					}
					pipe.SPublish(streamHopsKey, eventBytes)
				}
				pipe.Expire(streamHopsKey, NEWTTL)
				pipe.Exec()
			}
		}


	})

	return

	// if returnErr != nil {
	// 	return
	// }


	// pipe := r.OpenPipe()
	// // streamIdGet := pipe.SetNxGet(streamIdKey(streamKey), streamId)
	// // contractIdsKey := contractIdsKey(streamKey)
	// // setSize := pipe.SCard(contractIdsKey)
	// // setAdd := pipe.SAdd(contractIdsKey, contractId)
	// ```
	// local contract_stream_key = KEYS[1]
	// local set_key = KEYS[2]
	// local key_to_delete = KEYS[3]
	// local stream_id = ARGV[1]
	// local contract_id = ARGV[2]

	// local initial_size = redis.call('SCARD', set_key)

	// local changed = redis.call('SADD', set_key, contract_id)

	// if initial_size == 0 then
	// 	if changed then
	//     	redis.call('SET', key_to_delete, stream_id)
	//     end
	// else
	// 	stream_id = redis.call('GET', key_to_delete)
	// end

	// if changed then
	//     redis.call('SET', contract_stream_key, stream_key)
	// end

	// return {initial_size, changed}
	// ```
	// pipe.Exec()
	// if streamIdGet.Get() {
	// 	streamId = streamIdGet.Get()
	// }
	// changed := 0 < setAdd.Change()
	// created := 0 == setSize.Get()


	// notifyEventIds := map[server.Id]IntCmd{}
	// notify := map[server.Id][][]byte{}

	// if changed {
	// 	pipe := r.OpenPipe()
	// 	if created {
	// 		for sourceId, destinationId := range streamPairs(streamKey) {
	// 			streamHop := streamHop(sourceId, destinationId, streamId)
	// 			pipe.Sadd(clientHopsKey(sourceId), streamHop)
	// 			pipe.Sadd(clientHopsKey(destinationId), streamHop)
	// 			notify[sourceId] = append(notify[sourceId], streamHop)
	// 			notify[destinationId] = append(notify[destinationId], streamHop)
	// 		}
	// 	}
	// 	pipe.Expire(streamIdKey(streamKey), NEWTTL)
	// 	pipe.Expire(contractIdsKey(streamKey), NEWTTL)
	// 	for clientId := range streamIds(streamKey) {
	// 		notifyEventIds[clientId] = pipe.Incr(clientCounterKey(clientId))
	// 		pipe.Expire(clientHopsKey(clientId), NEWTTL)
	// 	}
	// 	pipe.Exec()
	// }

	// for clientId, streamHops := range notify {
	// 	eventId := notifyEventIds[clientId].Int()
	// 	event := &StreamHopEvent{
	// 		EventId: eventId,
	// 		StreamHopEventType: StreamHopEventTypeAdded,
	// 		StreamHops: streamHops,
	// 	}
	// 	r.SPublish(clientStreamEvents(clientId), eventBytes)
	// }

	// script {
	// 	get stream id for key
	// 	if no stream id, add stream id
	// 	if contract id not in set, add
	// 	get set size
	// }

	// if added {
	// 	if set size == 1 {
	// 		// must be done is separate commits, since the key spans cluster
	// 		add stream hop for each client id
	// 		notify active stream changed
	// 	}
	// 	reset ttl on stream key keys
	// 	reset ttl on client id key
	// }

}

func RemoveFromStream(contractId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		streamKeyBytes, err := r.Get(contractStreamKey(contractId)).Bytes()
		if err == redis.Nil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamKey := streamKey(streamKeyBytes)

		// note the keys in eval have to be in the same hash slot
		result, err := r.Eval(
			```
			local stream_id_key = KEYS[1]
			local stream_contracts_key = KEYS[2]
			local contract_id = ARGV[1]

			local changed = redis.call('SREM', stream_contracts_key, contract_id)
			local final_size = redis.call('SCARD', stream_contracts_key)

			if final_size == 0 then
				if changed then
			    	redis.call('DEL', stream_id_key)
			    end
			end

			return {final_size, changed}
			```,
			[]string{
				streamIdKey(streamKey),
				streamContractsKey(streamKey),
			},
			contractId.Bytes(),
		)
		if err != nil {
			panic(err)
		}
		values := result.Slice()

		finalSize := int(values[0])
		changed := bool(values[1])

		if changed {
			pipe := r.TxPipeline()
			pipe.Del(contractStreamKey(contractId), streamKey)
			pipe.Del(contractStreamId(contractId), streamId)
			pipe.Exec()

			if finalSize == 0 {
				for clientId, edges := range streamKey.Edges() {
					streamHopsKey := clientStreamHopsKey(clientId)
					pipe := r.TxPipeline()
					
					eventId, err := r.Incr(clientEventIdKey(clientId)).Int64()
					if err != nil {
						panic(err)
					}
					event := &StreamHopEvent{
						StreamHopEventType: StreamHopEventTypeRemoved,
						EventId: eventId,
					}

					if edges[0] != nil {
						streamHop := newStreamHop(*edges[0], clientId, streamId)
						pipe.Sdel(streamHopsKey, streamHop)
						event.StreamHops = append(event.StreamHops, streamHop)
					}
					if edges[1] != nil {
						streamHop := newStreamHop(clientId, *edges[1], streamId)
						pipe.Sdel(streamHopsKey, streamHop)
						event.StreamHops = append(event.StreamHops, streamHop)
					}

					pipe.SPublish(streamHopsKey, eventBytes)

					pipe.Exec()
				}
			}
		}
	})

	return


	// script {
	// 	remove contract id
	// 	get set size
	// 	get stream id
	// 	if set size 0, remove stream id
	// }

	// if removed {
	// 	if set size == 0 {
	// 		// must be done is separate commits, since the key spans cluster
	// 		remove stream hop for each client id
	// 		notify active stream changed
	// 	}
	// }
}


func GetStreamIdForContract(contractId server.Id) (streamId *server.Id, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		streamIdBytes, err := r.Get(contractStreamId(contractId)).Bytes()
		if err == redis.Nil {
			return
		}
		if err != nil {
			panic(err)
		}
		streamId_ := server.Id(streamIdBytes)
		streamId = &streamId_
	})
	return
}


func GetStreamHops(clientId server.Id) (resetEvent *StreamHopEvent) {
	server.Redis(ctx, func(r server.RedisClient) {
		pipe := r.TxPipeline()
		streamHopsCmd := pipe.SMembers(clientHopsKey(clientId))
		eventIdCmd := pipe.Get(clientCounterKey(clientId))
		pipe.Exec()

		streamHopStrs, err := streamHopsCmd.Result()
		if err == redis.Nil {
			return
		}
		if err != nil {
			panic(err)
		}
		eventId, err := streamHopsCmd.Int64()
		if err == redis.Nil {
			return
		}
		if err != nil {
			panic(err)
		}

		resetEvent = &StreamHopEvent{
			StreamHopEventType: StreamHopEventTypeReset,
			EventId: eventId,
		}
		for _, streamHopStr := range streamHopStrs {
			streamHop := streamHop([]byte(streamHopStr))
			resetEvent.StreamHops = append(resetEvent.StreamHops, resetEvent.Path())
		}
	})
	return
}


type StreamHopEventType int
const (
	StreamHopEventTypeAdded StreamHopEventType = 1
	StreamHopEventTypeRemoved StreamHopEventType = 2
	StreamHopEventTypeReset StreamHopEventType = 3
)

type StreamHopEvent struct {
	EventId int64
	// add, remove, reset
	StreamHopEventType StreamHopEventType
	StreamHops []TransferPath
}


type StreamHopListener struct {
	ctx context.Context
	cancel context.CancelFunc
	clientId server.Id
	callback func(*StreamHopEvent)
}

func NewStreamHopListener(ctx context.Context, clientId server.Id, callback func(*StreamHopEvent)) *StreamHopListener {
	cancelCtx, cancel := context.WithCancel(ctx)

	shl := &StreamHopListener{
		ctx: cancelCtx,
		cancel: cancel,
		clientId: clientId,
		callback: callback,
	}
	go server.HandleError(shl.run)
	return shl
}

func (self *StreamHopListener) run() {
	defer self.cancel()

	messages, unsub := server.Subscribe(self.ctx, clientStreamHopEvents(self.clientId))
	defer unsub()

	var eventId int64

	for {
		select {
		case <- self.ctx.Done():
			return
		case m := <- messages:
			switch m.(type) {
			case server.RedisSubscription:
				if m.Kind == "subscribe" {
					if resetEvent := GetStreamHops(clientId); resetEvent != nil {
						self.callback(resetEvent)
						eventId = resetEventId.EventId
					}
				}
			case server.RedisMessage:
				var event StreamHopEvent
				err := gob.Unmarshal([]byte(m.Payload), &event)
				if err == nil {
					if resetEventId < event.EventId {
						if event.EventId < eventId {
							// events delivered out of order, reset	
							if resetEvent := GetStreamHops(clientId); resetEvent != nil {
								self.callback(resetEvent)
								eventId = resetEventId.EventId
							}
						} else {
							eventId = event.EventId
							self.callback(event)
						}
					}
				}
			}
		}
	}
}

func (self *StreamHopListener) Close() {
	self.cancel()
}


