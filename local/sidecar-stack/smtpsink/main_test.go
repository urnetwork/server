package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// A sink serving on a loopback port of the system's choosing, and the channel its ServeSmtp returns on.
func startTestSink(t *testing.T, settings *SmtpSinkSettings, startTls bool) (*SmtpSink, string, chan error) {
	t.Helper()
	sink, err := NewSmtpSink(context.Background(), settings, startTls)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() {
		served <- sink.ServeSmtp(listener)
	}()
	t.Cleanup(sink.Close)
	return sink, listener.Addr().String(), served
}

// A raw session past the greeting.
func dialTestSink(t *testing.T, address string) (net.Conn, *textproto.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	text := textproto.NewConn(conn)
	if _, _, err := text.ReadResponse(220); err != nil {
		t.Fatal(err)
	}
	return conn, text
}

func testSinkCommand(t *testing.T, text *textproto.Conn, wantCode int, format string, args ...any) string {
	t.Helper()
	if err := text.PrintfLine(format, args...); err != nil {
		t.Fatal(err)
	}
	_, message, err := text.ReadResponse(wantCode)
	if err != nil {
		t.Fatalf("%s: %v", fmt.Sprintf(format, args...), err)
	}
	return message
}

// The sink takes a message a sending MTA hands it under STARTTLS and keeps it as sent, dot stuffing undone,
// and the control port lists it and forgets it.
func TestSmtpSinkTakesAMessage(t *testing.T) {
	sink, address, _ := startTestSink(t, DefaultSmtpSinkSettings(), true)

	client, err := smtp.Dial(address)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Hello("o1.out.report.example"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := client.Extension("STARTTLS"); !ok {
		t.Fatal("the sink offers no STARTTLS")
	}
	// the certificate is a throwaway no client can chain, which a local sender is told to accept
	if err := client.StartTLS(&tls.Config{InsecureSkipVerify: true, ServerName: "sink.mail.example"}); err != nil {
		t.Fatal(err)
	}
	if state, ok := client.TLSConnectionState(); !ok || len(state.PeerCertificates) != 1 || state.PeerCertificates[0].DNSNames[0] != "sink.mail.example" {
		t.Fatalf("tls state = %+v", state)
	}
	if ok, _ := client.Extension("STARTTLS"); ok {
		t.Error("the sink offers STARTTLS again under TLS")
	}
	if err := client.Mail("sender@ur.local.example"); err != nil {
		t.Fatal(err)
	}
	for _, recipient := range []string{"one@sink.mail.example", "two@sink.mail.example"} {
		if err := client.Rcpt(recipient); err != nil {
			t.Fatal(err)
		}
	}
	body := "From: sender@ur.local.example\r\nSubject: sink\r\n\r\nfirst line\r\n.a line that starts with a dot\r\n"
	writer, err := client.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Quit(); err != nil {
		t.Fatal(err)
	}

	messages := sink.Messages()
	if len(messages) != 1 {
		t.Fatalf("the sink kept %d messages", len(messages))
	}
	message := messages[0]
	// textproto's dot reader hands lines back ending in \n
	if want := strings.ReplaceAll(body, "\r\n", "\n"); string(message.Data) != want {
		t.Errorf("data = %q, want %q", message.Data, want)
	}
	if message.Index != 1 || message.Helo != "o1.out.report.example" || !message.Tls || message.From != "sender@ur.local.example" ||
		strings.Join(message.Recipients, " ") != "one@sink.mail.example two@sink.mail.example" || !strings.HasPrefix(message.RemoteAddress, "127.0.0.1:") {
		t.Errorf("message = %+v", message)
	}

	control := httptest.NewServer(sink.ControlHandler())
	defer control.Close()
	response, err := http.Get(control.URL + "/messages")
	if err != nil {
		t.Fatal(err)
	}
	listed := []*SmtpSinkMessage{}
	err = json.NewDecoder(response.Body).Decode(&listed)
	response.Body.Close()
	if err != nil || len(listed) != 1 || string(listed[0].Data) != string(message.Data) || listed[0].From != message.From {
		t.Fatalf("the control port lists %+v: %v", listed, err)
	}
	request, err := http.NewRequest(http.MethodDelete, control.URL+"/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	status := smtpSinkStatus{MessageCount: -1}
	err = json.NewDecoder(response.Body).Decode(&status)
	response.Body.Close()
	if err != nil || status.MessageCount != 0 || len(sink.Messages()) != 0 {
		t.Fatalf("after the delete the sink holds %+v: %v", status, err)
	}
	response, err = http.Post(control.URL+"/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a post to the control port answers %d", response.StatusCode)
	}
}

// The sink holds a session to SMTP's order and its own limits, relays nothing and offers no AUTH: each
// refusal answers its code and keeps nothing, and the session goes on to deliver.
func TestSmtpSinkRefusesWhatIsOutOfOrderOrPastItsLimits(t *testing.T) {
	settings := DefaultSmtpSinkSettings()
	settings.MessageMaxBytes = 64
	settings.RecipientMaxCount = 2
	settings.MessageMaxCount = 2
	sink, address, _ := startTestSink(t, settings, false)
	_, text := dialTestSink(t, address)

	testSinkCommand(t, text, 503, "MAIL FROM:<a@ur.local.example>")
	testSinkCommand(t, text, 501, "EHLO")
	if extensions := testSinkCommand(t, text, 250, "EHLO o1.out.report.example"); !strings.Contains(extensions, "SIZE 64") || strings.Contains(extensions, "STARTTLS") || strings.Contains(extensions, "AUTH") {
		t.Errorf("extensions = %q", extensions)
	}
	testSinkCommand(t, text, 502, "STARTTLS")
	testSinkCommand(t, text, 502, "AUTH PLAIN AGEAYg==")
	testSinkCommand(t, text, 502, "VRFY a")
	testSinkCommand(t, text, 503, "RCPT TO:<b@sink.mail.example>")
	testSinkCommand(t, text, 503, "DATA")
	testSinkCommand(t, text, 501, "MAIL FROM:a@ur.local.example")
	testSinkCommand(t, text, 250, "MAIL FROM:<a@ur.local.example> SIZE=10")
	testSinkCommand(t, text, 503, "MAIL FROM:<a@ur.local.example>")
	testSinkCommand(t, text, 503, "DATA")
	testSinkCommand(t, text, 501, "RCPT TO:<>")
	testSinkCommand(t, text, 250, "RCPT TO:<b@sink.mail.example>")
	testSinkCommand(t, text, 250, "rcpt to:<c@sink.mail.example>")
	testSinkCommand(t, text, 452, "RCPT TO:<d@sink.mail.example>")

	// a message past the limit is read to its end, refused and not kept
	testSinkCommand(t, text, 354, "DATA")
	testSinkCommand(t, text, 552, "%s\r\n.", strings.Repeat("x", 65))
	if len(sink.Messages()) != 0 {
		t.Fatalf("the sink kept a refused message: %+v", sink.Messages())
	}
	// the refusal ended the transaction
	testSinkCommand(t, text, 503, "RCPT TO:<b@sink.mail.example>")

	// a bounce, with the null path, at the limit exactly: 62 bytes and the line's end
	for index := 1; index <= 3; index += 1 {
		testSinkCommand(t, text, 250, "MAIL FROM:<>")
		testSinkCommand(t, text, 250, "RCPT TO:<b@sink.mail.example>")
		testSinkCommand(t, text, 250, "RSET")
		testSinkCommand(t, text, 503, "DATA")
		testSinkCommand(t, text, 250, "MAIL FROM:<>")
		testSinkCommand(t, text, 250, "RCPT TO:<b@sink.mail.example>")
		testSinkCommand(t, text, 354, "DATA")
		if taken := testSinkCommand(t, text, 250, "%d%s\r\n.", index, strings.Repeat("y", 62)); !strings.HasSuffix(taken, fmt.Sprintf("taken as %d", index)) {
			t.Errorf("taken = %q", taken)
		}
	}
	testSinkCommand(t, text, 250, "NOOP")
	testSinkCommand(t, text, 221, "QUIT")

	// the oldest message past the count kept is dropped and counted
	messages := sink.Messages()
	if len(messages) != 2 || messages[0].Index != 2 || messages[1].Index != 3 || messages[1].From != "" || messages[1].Tls ||
		string(messages[1].Data) != "3"+strings.Repeat("y", 62)+"\n" || len(messages[1].Recipients) != 1 {
		t.Errorf("messages = %+v", messages)
	}
	if status := sink.status(); status.MessageCount != 2 || status.DroppedCount != 1 {
		t.Errorf("status = %+v", status)
	}
}

// A command line past the limit closes the session with nothing kept.
func TestSmtpSinkClosesASessionOnALineTooLong(t *testing.T) {
	settings := DefaultSmtpSinkSettings()
	settings.LineMaxBytes = 128
	sink, address, _ := startTestSink(t, settings, false)
	conn, text := dialTestSink(t, address)
	// a line of the limit exactly, its line end included, is read
	testSinkCommand(t, text, 250, "EHLO %s", strings.Repeat("a", 128-len("EHLO \r\n")))
	if err := text.PrintfLine("EHLO %s", strings.Repeat("a", 128-len("EHLO \r\n")+1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := text.ReadResponse(500); err != nil {
		t.Errorf("a line past the limit: %v", err)
	}
	if line, err := text.ReadLine(); err != io.EOF {
		t.Errorf("the session went on: %q %v", line, err)
	}
	conn.Close()
	if len(sink.Messages()) != 0 {
		t.Errorf("messages = %+v", sink.Messages())
	}
}

// A message past twice the limit is not read to its end: the session closes, and nothing of it is kept.
func TestSmtpSinkClosesASessionOnAMessageFarPastTheLimit(t *testing.T) {
	settings := DefaultSmtpSinkSettings()
	settings.MessageMaxBytes = 64
	sink, address, _ := startTestSink(t, settings, false)
	conn, text := dialTestSink(t, address)
	testSinkCommand(t, text, 250, "EHLO o1.out.report.example")
	testSinkCommand(t, text, 250, "MAIL FROM:<a@ur.local.example>")
	testSinkCommand(t, text, 250, "RCPT TO:<b@sink.mail.example>")
	testSinkCommand(t, text, 354, "DATA")
	if err := text.PrintfLine("%s\r\n.", strings.Repeat("x", 200)); err != nil {
		t.Fatal(err)
	}
	if line, err := text.ReadLine(); err == nil {
		t.Errorf("the session went on: %q", line)
	}
	conn.Close()
	if len(sink.Messages()) != 0 {
		t.Errorf("messages = %+v", sink.Messages())
	}
}

// The sink stops cleanly: Close ends the listener, answers 421 to a session waiting between commands and to
// one in the middle of DATA, whose half message is not kept, keeps the message taken before, and returns
// only once every session has closed; nothing connects after.
func TestSmtpSinkStopsCleanly(t *testing.T) {
	sink, address, served := startTestSink(t, DefaultSmtpSinkSettings(), true)

	idleConn, idle := dialTestSink(t, address)
	testSinkCommand(t, idle, 250, "EHLO o1.out.report.example")

	sendingConn, sending := dialTestSink(t, address)
	testSinkCommand(t, sending, 250, "EHLO o2.out.report.example")
	testSinkCommand(t, sending, 250, "MAIL FROM:<a@ur.local.example>")
	testSinkCommand(t, sending, 250, "RCPT TO:<b@sink.mail.example>")
	testSinkCommand(t, sending, 354, "DATA")
	testSinkCommand(t, sending, 250, "whole\r\n.")
	testSinkCommand(t, sending, 250, "MAIL FROM:<a@ur.local.example>")
	testSinkCommand(t, sending, 250, "RCPT TO:<b@sink.mail.example>")
	testSinkCommand(t, sending, 354, "DATA")
	if err := sending.PrintfLine("half a message"); err != nil {
		t.Fatal(err)
	}

	// the close waits for each session's client to hang up, which each does once it has read the 421
	closed := make(chan struct{})
	go func() {
		sink.Close()
		close(closed)
	}()
	for name, session := range map[string]struct {
		conn net.Conn
		text *textproto.Conn
	}{"idle": {idleConn, idle}, "sending": {sendingConn, sending}} {
		if _, _, err := session.text.ReadResponse(421); err != nil {
			t.Errorf("the %s session was not answered 421: %v", name, err)
		}
		if _, err := session.text.ReadLine(); err != io.EOF {
			t.Errorf("the %s session stayed open: %v", name, err)
		}
		session.conn.Close()
	}
	<-closed
	if err := <-served; err != nil {
		t.Errorf("ServeSmtp returned %v", err)
	}
	messages := sink.Messages()
	if len(messages) != 1 || string(messages[0].Data) != "whole\n" {
		t.Errorf("messages = %+v", messages)
	}
	sink.stateLock.Lock()
	openConns, openListeners := len(sink.conns), len(sink.listeners)
	sink.stateLock.Unlock()
	if openConns != 0 || openListeners != 0 {
		t.Errorf("%d sessions and %d listeners outlive the close", openConns, openListeners)
	}
	if conn, err := net.Dial("tcp4", address); err == nil {
		conn.Close()
		t.Error("the sink still accepts after its close")
	}
	// a second close and a serve after the close are no-ops
	sink.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.ServeSmtp(listener); err != nil {
		t.Errorf("a serve after the close returned %v", err)
	}
}
