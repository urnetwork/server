package server

import (
	"github.com/jackc/pgx/v5/pgtype"
)

// The `timestamp` columns hold UTC without a zone (see NowUtc). pgx's stock
// TimestampCodec writes a time's wall clock and drops its location, so a time
// in any location but time.UTC was stored shifted by that location's offset:
// a time.Unix, time.UnixMilli or time.Now value in a process off UTC, a time
// parsed with an offset other than Z, or a time moved with In. Each such write
// needed its own UTC() call. This codec moves every finite time to UTC before
// the stock encoding, so a `timestamp` stores a time's instant whatever its
// location, pgtype.Timestamp values included. UTC times, and Local ones on a
// host that runs in UTC, encode exactly as before.
//
// Scans are the stock codec's: a stored value reads back as a UTC time.
// timestamptz already encodes the instant, and date takes the calendar day of
// the given time, so neither changes.
type pgUtcTimestampCodec struct {
	pgtype.TimestampCodec
}

func (self *pgUtcTimestampCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if _, ok := value.(pgtype.TimestampValuer); !ok {
		return nil
	}
	next := self.TimestampCodec.PlanEncode(m, oid, format, pgtype.Timestamp{})
	if next == nil {
		return nil
	}
	return &encodePlanUtcTimestamp{next: next}
}

type encodePlanUtcTimestamp struct {
	next pgtype.EncodePlan
}

func (self *encodePlanUtcTimestamp) Encode(value any, buf []byte) ([]byte, error) {
	timestamp, err := value.(pgtype.TimestampValuer).TimestampValue()
	if err != nil {
		return nil, err
	}
	if timestamp.Valid && timestamp.InfinityModifier == pgtype.Finite {
		timestamp.Time = timestamp.Time.UTC()
	}
	return self.next.Encode(timestamp, buf)
}

// Registers the UTC `timestamp` codec on a connection's type map, for
// `timestamp` and `timestamp[]`. Every pool connection gets it (AfterConnect in
// configurePgPoolLiveness).
func pgxRegisterUtcTimestampType(typeMap *pgtype.Map) {
	timestampType := &pgtype.Type{
		Name:  "timestamp",
		OID:   pgtype.TimestampOID,
		Codec: &pgUtcTimestampCodec{},
	}
	typeMap.RegisterType(timestampType)
	typeMap.RegisterType(&pgtype.Type{
		Name:  "_timestamp",
		OID:   pgtype.TimestampArrayOID,
		Codec: &pgtype.ArrayCodec{ElementType: timestampType},
	})
}
