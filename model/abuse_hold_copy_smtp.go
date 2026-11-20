package model

// smtp-3's rows in §21.9's copied_rows (ABUSE-M9-01; R-ABUSE-5, D-SMTP-10, I-13): smtp_send drops whole by the UTC
// day at 30 days (SMTP-M9-17's retention, through the family's Retire), so a hold copies a held network's send
// records existing at its placement before their day may go. The day's drop precondition (connectionRecordRangeHold)
// asks AbuseHoldCopyPending once the table is registered here, so a held day waits only until its hold's copy lands,
// and an unheld day drops on time. §21.9's production list reaches the same rows while they are kept, from M9
// (AbuseProductionRows' sends).
//
// A row is what smtp-3 keeps, and the copy keeps all of it and adds nothing: the MX's name and never its address,
// the recipient domain and counts and never a recipient, a Message-ID hash and never a subject or a record length
// (D-SMTP-10), so no third party's address or content exists to copy.
//
// The table's key is (send_day, send_id), and send_day is start_time's UTC day, so (start_time, send_id) orders its
// rows uniquely: the copy's cursor. send_id is 12 octets, read as text in bytea's hex form, whose order is the
// octets' own at one length.

import (
	"github.com/urnetwork/server"
)

// smtp-3's rows, which drop whole by the day: this task's registration. Only a binary linking MG9 holds the table.
func init() {
	RegisterHoldCopy(&HoldCopyTable{
		Table:         smtpSendTable,
		NetworkColumn: "network_id",
		TimeColumn:    "start_time",
		KeyColumn:     "send_id",
		Columns: []string{
			"send_day",
			"send_id",
			"network_id",
			"client_id",
			"sending_address",
			"recipient_domain",
			"mx_host",
			"start_time",
			"end_time",
			"byte_count",
			"message_id_hash",
			"x_ur_ref_key_id",
			"recipient_count",
			"message_byte_count",
			"tls_version",
			"tls_level",
			"result",
			"retry_of_send_id",
		},
		Linked: server.SidecarMg9Linked,
	})
}
