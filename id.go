package server

import (
	"fmt"

	"database/sql/driver"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/urnetwork/connect"
	operator "github.com/urnetwork/operator-core"
)

// Id is the server's 16-byte identifier: operator-core's Id (owner decision 251), which is this type
// moved there, so server and its extensions share one. It wraps connect.Id so the core implementation
// (parsing, formatting, comparison) lives in a single place, see ../connect, with the database/sql
// Scanner/Valuer and JSON codecs operator-core holds. The generators and parsers below stay here, and
// so does the pgx codec, registered for that type.
type Id = operator.Id

func NewId() Id {
	return Id(connect.NewId())
}

func IdFromBytes(idBytes []byte) (Id, error) {
	id, err := connect.IdFromBytes(idBytes)
	return Id(id), err
}

func RequireIdFromBytes(idBytes []byte) Id {
	return Id(connect.RequireIdFromBytes(idBytes))
}

func ParseId(idStr string) (Id, error) {
	id, err := connect.ParseId(idStr)
	return Id(id), err
}

func RequireParseId(idStr string) Id {
	id, err := ParseId(idStr)
	if err != nil {
		panic(err)
	}
	return id
}

func pgxRegisterIdType(typeMap *pgtype.Map) {
	// for bringyour, `uuid` pgtype maps to `Id`
	// in code, `*Id` is used for nullable values
	typeMap.RegisterType(&pgtype.Type{
		Name:  "uuid",
		OID:   pgtype.UUIDOID,
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

	return id.String(), nil
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
		return append(buf, v.String()...), nil
	case Id:
		return append(buf, v.String()...), nil
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

	var buf Id
	copy(buf[:], src)

	return scanner.ScanText(pgtype.Text{String: buf.String(), Valid: true})
}

type scanPlanTextAnyToIdScanner struct{}

func (scanPlanTextAnyToIdScanner) Scan(src []byte, dst any) error {
	switch v := dst.(type) {
	case **Id:
		if src == nil {
			*v = nil
			return nil
		}
		id, err := ParseId(string(src))
		if err != nil {
			return err
		}
		*v = &id
		return nil
	case *Id:
		if src == nil {
			return fmt.Errorf("Cannot scan a nil value into *Id (use **Id)")
		}
		id, err := ParseId(string(src))
		if err != nil {
			return err
		}
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
