//go:build sidecar_pending

package model

// DnsSuspendSettings (QD-702) on Postgres: the bound SuspendZone holds a reason to is network_dns_zone's column.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A zone suspension's reason is kept to network_dns_zone's column: DnsSuspendSettings' default is the column's
// width, a reason one byte past it is refused with nothing written, and one of that length is kept.
func TestDnsSuspendReasonMaxLengthIsTheColumns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root, networkId, _ := testDnsSuspendZone(t, ctx)
		maxLength := DefaultDnsSuspendSettings().ReasonMaxLength
		var width int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
					SELECT character_maximum_length::integer FROM information_schema.columns
					WHERE table_name = 'network_dns_zone' AND column_name = 'suspend_reason'
				`,
			).Scan(&width))
		})
		if maxLength != width {
			t.Fatalf("the reason's bound is %d, the column's %d", maxLength, width)
		}
		state, _ := GetDnsRootState(ctx, root)
		if err := SuspendZone(ctx, networkId, strings.Repeat("r", maxLength+1), nil); !errors.Is(err, ErrDnsSuspendReason) {
			t.Fatalf("a reason past the column answered %v", err)
		}
		requireDnsSerial(t, ctx, root, state.Serial)
		if err := SuspendZone(ctx, networkId, strings.Repeat("r", maxLength), nil); err != nil {
			t.Fatal(err)
		}
	})
}
