package server

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
	// the zones of the cases, where a system has no zone database
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgtype"
)

// Microseconds from the Unix epoch to 2000-01-01, PostgreSQL's epoch.
const pgTimestampEpochMicros = 946_684_800_000_000

// A `timestamp` is written as the UTC wall clock of a time's instant, in both
// of pgx's formats, whatever the time's location, and reads back as that
// instant in UTC. The stock codec wrote a time's own wall clock, so without
// pgxRegisterUtcTimestampType every case off UTC fails in any process zone.
func TestPgTimestampEncodesTheInstantInUtc(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	// half an hour before America/Chicago leaves daylight saving
	instant := time.Date(2026, 11, 1, 6, 30, 0, 123456789, time.UTC)
	parsed, err := time.Parse(time.RFC3339Nano, "2026-11-01T03:00:00.123456789-03:30")
	if err != nil {
		t.Fatal(err)
	}
	inTokyo := instant.In(tokyo)
	beforeEpoch := time.Date(1969, 7, 20, 20, 17, 40, 0, time.UTC)

	typeMap := pgtype.NewMap()
	pgxRegisterUtcTimestampType(typeMap)
	for _, c := range []struct {
		name  string
		value any
		want  time.Time
	}{
		{"a UTC time", instant, instant},
		{"a time in the process's zone", instant.Local(), instant},
		{"a time.UnixMilli time", time.UnixMilli(instant.UnixMilli()), instant.Truncate(time.Millisecond)},
		{"a fixed zone west of UTC", instant.In(time.FixedZone("", -5*60*60)), instant},
		{"a fixed zone east of UTC by a half hour", instant.In(time.FixedZone("", 5*60*60+30*60)), instant},
		{"an RFC 3339 offset", parsed, instant},
		{"America/Chicago in daylight saving", instant.In(chicago), instant},
		{"America/Chicago after daylight saving", instant.Add(time.Hour).In(chicago), instant.Add(time.Hour)},
		{"Asia/Tokyo", inTokyo, instant},
		{"a pointer", &inTokyo, instant},
		{"a pgtype.Timestamp", pgtype.Timestamp{Time: inTokyo, Valid: true}, instant},
		{"before the Unix epoch", beforeEpoch.In(time.FixedZone("", -4*60*60)), beforeEpoch},
	} {
		binaryValue, err := typeMap.Encode(pgtype.TimestampOID, pgtype.BinaryFormatCode, c.value, nil)
		if err != nil || len(binaryValue) != 8 {
			t.Fatalf("%s: binary encoding %x, %v", c.name, binaryValue, err)
		}
		wantMicros := c.want.Truncate(time.Microsecond).UnixMicro() - pgTimestampEpochMicros
		if micros := int64(binary.BigEndian.Uint64(binaryValue)); micros != wantMicros {
			t.Errorf("%s was written as %s, want %s", c.name, time.UnixMicro(micros+pgTimestampEpochMicros).UTC(), c.want.UTC())
		}
		textValue, err := typeMap.Encode(pgtype.TimestampOID, pgtype.TextFormatCode, c.value, nil)
		wantText := c.want.UTC().Truncate(time.Microsecond).Format("2006-01-02 15:04:05.999999")
		if err != nil || string(textValue) != wantText {
			t.Errorf("%s was written as text %q, %v; want %q", c.name, textValue, err, wantText)
		}
		for _, encoded := range []struct {
			format int16
			value  []byte
		}{{pgtype.BinaryFormatCode, binaryValue}, {pgtype.TextFormatCode, textValue}} {
			var scanned time.Time
			err := typeMap.Scan(pgtype.TimestampOID, encoded.format, encoded.value, &scanned)
			if err != nil || !scanned.Equal(c.want.Truncate(time.Microsecond)) || scanned.Location() != time.UTC {
				t.Errorf("%s read back in format %d as %s, %v; want %s", c.name, encoded.format, scanned, err, c.want.UTC())
			}
		}
	}

	// NULL and the infinities are written as the stock codec writes them
	stockMap := pgtype.NewMap()
	var nilTime *time.Time
	for _, value := range []any{
		nilTime,
		pgtype.Timestamp{},
		pgtype.Timestamp{Valid: true, InfinityModifier: pgtype.Infinity},
		pgtype.Timestamp{Valid: true, InfinityModifier: pgtype.NegativeInfinity},
	} {
		for _, format := range []int16{pgtype.BinaryFormatCode, pgtype.TextFormatCode} {
			got, err := typeMap.Encode(pgtype.TimestampOID, format, value, nil)
			want, wantErr := stockMap.Encode(pgtype.TimestampOID, format, value, nil)
			if err != nil || wantErr != nil || !bytes.Equal(got, want) || (got == nil) != (want == nil) {
				t.Errorf("%#v in format %d was written as %q, %v; want %q, %v", value, format, got, err, want, wantErr)
			}
		}
	}

	// timestamp[] elements, NULL among them
	for _, value := range []any{
		[]time.Time{instant.In(chicago), inTokyo, parsed},
		[]*time.Time{&inTokyo, nil},
	} {
		for _, format := range []int16{pgtype.BinaryFormatCode, pgtype.TextFormatCode} {
			encoded, err := typeMap.Encode(pgtype.TimestampArrayOID, format, value, nil)
			if err != nil {
				t.Fatalf("timestamp[] %v in format %d: %v", value, format, err)
			}
			var scanned []*time.Time
			if err := typeMap.Scan(pgtype.TimestampArrayOID, format, encoded, &scanned); err != nil {
				t.Fatalf("timestamp[] in format %d: %v", format, err)
			}
			if _, withNull := value.([]*time.Time); withNull {
				if len(scanned) != 2 || scanned[0] == nil || !scanned[0].Equal(instant.Truncate(time.Microsecond)) || scanned[1] != nil {
					t.Errorf("timestamp[] with a NULL in format %d read back as %v", format, scanned)
				}
				continue
			}
			if len(scanned) != 3 {
				t.Fatalf("timestamp[] in format %d read back %d elements", format, len(scanned))
			}
			for index, element := range scanned {
				if element == nil || !element.Equal(instant.Truncate(time.Microsecond)) || element.Location() != time.UTC {
					t.Errorf("timestamp[] element %d in format %d read back as %v, want %s", index, format, element, instant.Truncate(time.Microsecond))
				}
			}
		}
	}

	// timestamptz already encodes the instant and date takes the given time's
	// calendar day; neither changes
	for _, oid := range []uint32{pgtype.TimestamptzOID, pgtype.DateOID} {
		for _, format := range []int16{pgtype.BinaryFormatCode, pgtype.TextFormatCode} {
			got, err := typeMap.Encode(oid, format, inTokyo, nil)
			want, wantErr := stockMap.Encode(oid, format, inTokyo, nil)
			if err != nil || wantErr != nil || !bytes.Equal(got, want) {
				t.Errorf("oid %d in format %d was written as %q, %v; want the stock %q, %v", oid, format, got, err, want, wantErr)
			}
		}
	}
}
