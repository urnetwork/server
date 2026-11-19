package main

import (
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// The complete partition walk can revisit one serialized record millions of
// times. Sharing its immutable decoded values avoids retaining another copy of
// nested registration/risk evidence for every intersecting origin. Each reader
// gets its own cache because MMDB offsets are local to that exact database.
// FIFO eviction bounds the optimization; it never omits a source record.
const subscriberRecordCacheLimit = 65536

type subscriberRecordCache struct {
	records map[uintptr]mmdbtype.Map
	order   []uintptr
	next    int
	hits    int64
	misses  int64
}

func (c *subscriberRecordCache) decode(result mmdb.Result) (mmdbtype.Map, error) {
	if err := result.Err(); err != nil {
		return nil, err
	}
	offset := result.Offset()
	if record, found := c.records[offset]; found {
		c.hits++
		return record, nil
	}
	var record mmdbtype.Map
	if err := result.Decode(&record); err != nil {
		return nil, err
	}
	c.misses++
	if c.records == nil {
		c.records = make(map[uintptr]mmdbtype.Map)
	}
	if len(c.order) < subscriberRecordCacheLimit {
		c.order = append(c.order, offset)
	} else {
		delete(c.records, c.order[c.next])
		c.order[c.next] = offset
		c.next = (c.next + 1) % subscriberRecordCacheLimit
	}
	c.records[offset] = record
	return record, nil
}
