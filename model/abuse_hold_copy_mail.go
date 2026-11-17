package model

// mx-2's rows in §21.9's copied_rows (ABUSE-M7-01; R-ABUSE-5, D-MAIL-9, I-13): mail_connection_record drops whole
// at 24 h, like tls-3's records, so a hold copies a held network's rows existing at its placement before their hour
// may go. Its drop precondition (connectionRecordRangeHold) asks AbuseHoldCopyPending once the table is registered
// here, so a held hour waits only until its hold's copy lands, and an unheld hour drops on time.
//
// A row is what mx-2 keeps: the receiving network, the session's start, its bytes and a result word. It holds no
// sender address and no burner label, and the copy adds none. The router's closed-handshake memory, a keyed prefix
// held 1 h with no network, is no row and stays out of every hold.
//
// The table has no key of its own: its rows are only ever inserted, so a row's tuple id stands while a copy reads,
// and with start_time, the column the hours partition on, it orders the rows uniquely, two rows of one start sitting
// in one partition. The copy's cursor reads it as text.

import (
	"github.com/urnetwork/server"
)

// mx-2's rows, which drop whole at 24 h: this task's registration. Only a binary linking MG7 holds the table.
func init() {
	RegisterHoldCopy(&HoldCopyTable{
		Table:         mailConnectionRecordTable,
		NetworkColumn: "network_id",
		TimeColumn:    "start_time",
		KeyColumn:     "ctid",
		Columns:       []string{"network_id", "start_time", "byte_count", "result"},
		Linked:        server.SidecarMg7Linked,
	})
}
