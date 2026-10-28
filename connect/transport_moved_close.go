// The moved close (connect/MOVE.md §5.1, §5.6): the close a compulsory move's
// deadline sends the client's transports on this process when the client's
// resident is still here at the deadline. It follows the client limit close
// (4001, provider_intent.go) and the authorization closes (4002-4005,
// session_notifications.go):
//   - H1 (WebSocket and H1+ framed): one 5 byte binary message, transport
//     control 3 followed by the big-endian uint32 close reason 6, then on
//     WebSocket a close frame with code 4006, then the socket closes.
//   - H3 (QUIC, also DNS-carried): the connection closes with application
//     error code 4006.
//
// A client that predates moves reads an unknown reason or code as an ordinary
// close, and redials at once since its connection is older than its reconnect
// timer.
package connect

import (
	"encoding/binary"
	"time"

	"github.com/gorilla/websocket"
	quic "github.com/quic-go/quic-go"

	"github.com/urnetwork/connect"
)

// the close reason in the H1 close message
const closeReasonMoved uint32 = 6

// the WebSocket close code and QUIC application error code of the moved close
const movedCloseCode = 4006

const movedCloseText = "moved"

// The H1 close message: transport control 3 and the moved reason.
func movedControlMessage() []byte {
	message := make([]byte, 5)
	message[0] = transportControlClose
	binary.BigEndian.PutUint32(message[1:5], closeReasonMoved)
	return message
}

// Sends the moved close on an H1 connection. The caller must be the
// connection's only writer and closes the connection afterwards.
func writeConnectH1Moved(ws connect.H1MessageConn, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if err := ws.SetWriteDeadline(deadline); err != nil {
		return err
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, movedControlMessage()); err != nil {
		return err
	}
	if websocketConn, ok := connect.H1WebSocketConn(ws); ok {
		// best effort: the control message above is the signal
		return websocketConn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(movedCloseCode, movedCloseText),
			deadline,
		)
	}
	return nil
}

// Sends the moved close on an H3 connection, closing it.
func closeConnectQuicMoved(conn *quic.Conn) error {
	return conn.CloseWithError(quic.ApplicationErrorCode(movedCloseCode), movedCloseText)
}
