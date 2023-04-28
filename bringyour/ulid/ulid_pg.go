package ulid

import (
	"github.com/jackc/pgx/v5/pgtype"
)


// converts to/from pgtype.UUID


func FromPg(UUID pgtype.UUID) *ULID {
	if !UUID.Valid {
		return nil
	} else {
		ulid := ULID(UUID.Bytes)
		return &ulid
	}
}

// fixme handle ULID type also?
func ToPg(ULID *ULID) pgtype.UUID {
	if ULID == nil {
		return pgtype.UUID{
			Valid: false,
		}
	} else {
		return pgtype.UUID{
			Bytes: [16]byte(*ULID),
			Valid: true,
		}
	}
}

