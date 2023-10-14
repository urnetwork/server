package bringyour

import (
	"fmt"
	"bytes"
	"encoding/hex"
	"errors"

	"database/sql/driver"

	"github.com/oklog/ulid/v2"
	"github.com/jackc/pgx/v5/pgtype"
	
)


type Id [16]byte

func NewId() Id {
	return Id(ulid.Make())
}

func IdFromSlice(idBytes []byte) (Id, error) {
	if len(idBytes) != 16 {
		return Id{}, errors.New("Id must be 16 bytes")
	}
	return Id(idBytes), nil
}

func ParseId(idStr string) (id Id, err error) {
	return parseUUID(idStr) 
}

func (self *Id) Less(b Id) bool {
	return self.Cmp(b) < 0
}

func (self *Id) Cmp(b Id) int {
	for i, v := range self {
		if v < b[i] {
			return -1
		}
		if b[i] < v {
			return 1
		}
	}
	return 0
}

func (self *Id) Bytes() []byte {
	return self[0:16]
}

func (self *Id) String() string {
	return encodeUUID(*self)
}

// Scan implements the database/sql Scanner interface.
func (dst *Id) Scan(src any) error {
	if src == nil {
		return fmt.Errorf("Scan with nil source not supported by Id (use *Id)")
	}

	switch src := src.(type) {
	case string:
		buf, err := parseUUID(src)
		if err != nil {
			return err
		}
		*dst = buf
		return nil
	}

	return fmt.Errorf("cannot scan %T", src)
}

// Value implements the database/sql/driver Valuer interface.
func (src *Id) Value() (driver.Value, error) {
	return encodeUUID(*src), nil
}

func (src *Id) MarshalJSON() ([]byte, error) {
	var buff bytes.Buffer
	buff.WriteByte('"')
	buff.WriteString(encodeUUID(*src))
	buff.WriteByte('"')
	return buff.Bytes(), nil
}

func (dst *Id) UnmarshalJSON(src []byte) error {
	if bytes.Equal(src, []byte("null")) {
		return fmt.Errorf("Unmarshal with nil source not supported by Id (use *Id)")
	}
	if len(src) != 38 {
		return fmt.Errorf("invalid length for UUID: %v", len(src))
	}
	buf, err := parseUUID(string(src[1 : len(src)-1]))
	if err != nil {
		return err
	}
	*dst = buf
	return nil
}


// parseUUID converts a string UUID in standard form to a byte array.
func parseUUID(src string) (dst [16]byte, err error) {
	switch len(src) {
	case 36:
		src = src[0:8] + src[9:13] + src[14:18] + src[19:23] + src[24:]
	case 32:
		// dashes already stripped, assume valid
	default:
		// assume invalid.
		return dst, fmt.Errorf("cannot parse UUID %v", src)
	}

	buf, err := hex.DecodeString(src)
	if err != nil {
		return dst, err
	}

	copy(dst[:], buf)
	return dst, err
}


func encodeUUID(src [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", src[0:4], src[4:6], src[6:8], src[8:10], src[10:16])
}


func pgxRegisterIdType(typeMap *pgtype.Map) {
	// for bringyour, `uuid` pgtype maps to `Id`
	// in code, `*Id` is used for nullable values 
	typeMap.RegisterType(&pgtype.Type{
		Name: "uuid",
		OID: pgtype.UUIDOID,
		Codec: &PgIdCodec{},
	})
}


type PgIdCodec struct{}

func (self *PgIdCodec) FormatSupported(format int16) bool {
	switch format {
	case pgtype.TextFormatCode, pgtype.BinaryFormatCode:
		return true
	default:
		return false
	}
}

func (self *PgIdCodec) PreferredFormat() int16 {
	return pgtype.BinaryFormatCode
}

func (self *PgIdCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	switch value.(type) {
	case Id, *Id:
	default:
		return nil
	}

	switch format {
	case pgtype.BinaryFormatCode:
		return encodePlanUUIDCodecBinaryIdValuer{}
	case pgtype.TextFormatCode:
		return encodePlanUUIDCodecTextIdValuer{}
	}

	return nil
}

func (self *PgIdCodec) PlanScan(m *pgtype.Map, oid uint32, format int16, target any) pgtype.ScanPlan {
	switch format {
	case pgtype.BinaryFormatCode:
		switch target.(type) {
		case *Id, **Id:
			return scanPlanBinaryUUIDToIdScanner{}
		case pgtype.TextScanner:
			return scanPlanBinaryUUIDToTextScanner{}
		}
	case pgtype.TextFormatCode:
		switch target.(type) {
		case *Id, **Id:
			return scanPlanTextAnyToIdScanner{}
		}
	}

	return nil
}

func (self *PgIdCodec) DecodeDatabaseSQLValue(m *pgtype.Map, oid uint32, format int16, src []byte) (driver.Value, error) {
	if src == nil {
		return nil, nil
	}

	var id Id
	err := codecScan(self, m, oid, format, src, &id)
	if err != nil {
		return nil, err
	}

	return encodeUUID(id), nil
}

func (self *PgIdCodec) DecodeValue(m *pgtype.Map, oid uint32, format int16, src []byte) (any, error) {
	if src == nil {
		return nil, nil
	}

	var id Id
	err := codecScan(self, m, oid, format, src, &id)
	if err != nil {
		return nil, err
	}
	return [16]byte(id), nil
}


type encodePlanUUIDCodecBinaryIdValuer struct{}

func (encodePlanUUIDCodecBinaryIdValuer) Encode(value any, buf []byte) ([]byte, error) {
	switch v := value.(type) {
	case *Id:
		if v == nil {
			return nil, nil
		}
		return append(buf, v[:]...), nil
	case Id:
		return append(buf, v[:]...), nil
	default:
		return nil, fmt.Errorf("Unknown value %T (expected Id or *Id)", v)
	}
}


type encodePlanUUIDCodecTextIdValuer struct{}

func (encodePlanUUIDCodecTextIdValuer) Encode(value any, buf []byte) ([]byte, error) {
	switch v := value.(type) {
	case *Id:
		if v == nil {
			return nil, nil
		}
		return append(buf, encodeUUID(*v)...), nil
	case Id:
		return append(buf, encodeUUID(v)...), nil
	default:
		return nil, fmt.Errorf("Unknown value %T (expected Id or *Id)", v)
	}
}


type scanPlanBinaryUUIDToIdScanner struct{}

func (scanPlanBinaryUUIDToIdScanner) Scan(src []byte, dst any) error {
	switch v := dst.(type) {
	case **Id:
		if src == nil {
			*v = nil
			return nil
		}
		if len(src) != 16 {
			return fmt.Errorf("invalid length for UUID: %v", len(src))
		}
		id := Id{}
		copy(id[:], src)
		*v = &id
		return nil
	case *Id:
		if src == nil {
			return fmt.Errorf("Cannot scan a nil value into *Id (use **Id)")
		}
		if len(src) != 16 {
			return fmt.Errorf("invalid length for UUID: %v", len(src))
		}
		id := Id{}
		copy(id[:], src)
		*v = id
		return nil
	default:
		return fmt.Errorf("Unknown value %T (expected *Id or **Id)", v)
	}
}


type scanPlanBinaryUUIDToTextScanner struct{}

func (scanPlanBinaryUUIDToTextScanner) Scan(src []byte, dst any) error {
	scanner := dst.(pgtype.TextScanner)

	if src == nil {
		return scanner.ScanText(pgtype.Text{})
	}

	if len(src) != 16 {
		return fmt.Errorf("invalid length for UUID: %v", len(src))
	}

	var buf [16]byte
	copy(buf[:], src)

	return scanner.ScanText(pgtype.Text{String: encodeUUID(buf), Valid: true})
}


type scanPlanTextAnyToIdScanner struct{}

func (scanPlanTextAnyToIdScanner) Scan(src []byte, dst any) error {
	switch v := dst.(type) {
	case **Id:
		if src == nil {
			*v = nil
			return nil
		}
		buf, err := parseUUID(string(src))
		if err != nil {
			return err
		}
		id := Id(buf)
		*v = &id
		return nil
	case *Id:
		if src == nil {
			return fmt.Errorf("Cannot scan a nil value into *Id (use **Id)")
		}
		buf, err := parseUUID(string(src))
		if err != nil {
			return err
		}
		id := Id(buf)
		*v = id
		return nil
	default:
		return fmt.Errorf("Unknown value %T (expected *Id or **Id)", v)
	}
}


// copied from `pgtype.codecScan`
func codecScan(codec pgtype.Codec, m *pgtype.Map, oid uint32, format int16, src []byte, dst any) error {
	scanPlan := codec.PlanScan(m, oid, format, dst)
	if scanPlan == nil {
		return fmt.Errorf("PlanScan did not find a plan")
	}
	return scanPlan.Scan(src, dst)
}


