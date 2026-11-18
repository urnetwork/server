// smtpsink is the local tier's SMTP sink (sidecar DESIGN §19.11's local row; OPS-M9-01): a destination MX that
// takes every message handed to it, keeps it in memory and delivers it nowhere, so a local smtpedge has a
// port 25 to send to and a test can read what arrived.
//
//	smtpsink -smtp :2525 -control :8025 -hostname sink.mail.example
//
// It speaks enough SMTP for a sending MTA: EHLO and HELO, STARTTLS on a throwaway certificate made at start,
// MAIL, RCPT, DATA, RSET, NOOP and QUIT, and it has no AUTH and no relay. The control port lists what arrived:
//
//	GET    /status      {"message_count": n, "dropped_count": n}
//	GET    /messages    every kept message, oldest first
//	DELETE /messages    forgets them
//
// On SIGTERM or SIGINT it stops accepting, answers 421 to each open session, and exits 0 once they have
// closed. A message counts as taken only once its DATA was answered 250.
//
// It is a test fixture for loopback: it keeps what it takes in memory, in the clear, and nothing in it is
// fit to face a network.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type SmtpSinkSettings struct {
	// The name the sink greets as and its throwaway certificate names.
	Hostname string
	// The largest message taken, which EHLO's SIZE gives; a larger one is answered 552 and not kept.
	MessageMaxBytes int
	// How many messages are kept; past it the oldest is dropped and counted.
	MessageMaxCount int
	// How many recipients one message may name before RCPT is answered 452.
	RecipientMaxCount int
	// The longest command line read, its line end included; a longer one is answered 500 and closes the
	// session.
	LineMaxBytes int
	// How long a session may sit between commands, or within DATA, before it is closed.
	CommandTimeout time.Duration
	// How long a session the sink ends waits for its client to hang up, discarding what it still sends,
	// so the client reads the closing answer ahead of the close.
	CloseDrainTimeout time.Duration
	// How long the throwaway certificate stays valid from the sink's start.
	CertificateLifetime time.Duration
}

func DefaultSmtpSinkSettings() *SmtpSinkSettings {
	return &SmtpSinkSettings{
		Hostname:            "sink.mail.example",
		MessageMaxBytes:     32 * 1024 * 1024,
		MessageMaxCount:     1024,
		RecipientMaxCount:   128,
		LineMaxBytes:        4096,
		CommandTimeout:      60 * time.Second,
		CloseDrainTimeout:   2 * time.Second,
		CertificateLifetime: 30 * 24 * time.Hour,
	}
}

// One message the sink took, as the control port lists it.
type SmtpSinkMessage struct {
	// counts from 1 over the sink's life, so a dropped message leaves a gap
	Index         int       `json:"index"`
	RemoteAddress string    `json:"remote_address"`
	Helo          string    `json:"helo"`
	Tls           bool      `json:"tls"`
	From          string    `json:"from"`
	Recipients    []string  `json:"recipients"`
	Data          []byte    `json:"data"`
	ReceivedTime  time.Time `json:"received_time"`
}

type smtpSinkStatus struct {
	MessageCount int `json:"message_count"`
	DroppedCount int `json:"dropped_count"`
}

type SmtpSink struct {
	ctx      context.Context
	cancel   context.CancelFunc
	settings *SmtpSinkSettings
	// nil when the sink offers no STARTTLS
	tlsConfig *tls.Config

	stateLock    sync.Mutex
	messages     []*SmtpSinkMessage
	messageIndex int
	droppedCount int
	listeners    map[net.Listener]bool
	conns        map[net.Conn]bool
	sessions     sync.WaitGroup
}

// A sink with no listener yet. With startTls it makes its throwaway certificate here.
func NewSmtpSink(ctx context.Context, settings *SmtpSinkSettings, startTls bool) (*SmtpSink, error) {
	cancelCtx, cancel := context.WithCancel(ctx)
	sink := &SmtpSink{
		ctx:       cancelCtx,
		cancel:    cancel,
		settings:  settings,
		listeners: map[net.Listener]bool{},
		conns:     map[net.Conn]bool{},
	}
	if startTls {
		certificate, err := throwawayCertificate(settings)
		if err != nil {
			cancel()
			return nil, err
		}
		sink.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{*certificate},
			MinVersion:   tls.VersionTLS12,
		}
	}
	return sink, nil
}

// A self-signed certificate for the sink's name, which no client should trust beyond a local test.
func throwawayCertificate(settings *SmtpSinkSettings) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: settings.Hostname},
		DNSNames:              []string{settings.Hostname},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(settings.CertificateLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// Serves SMTP sessions from listener until the sink closes, and returns nil then.
func (self *SmtpSink) ServeSmtp(listener net.Listener) error {
	self.stateLock.Lock()
	if self.ctx.Err() != nil {
		self.stateLock.Unlock()
		listener.Close()
		return nil
	}
	self.listeners[listener] = true
	self.stateLock.Unlock()
	defer func() {
		listener.Close()
		self.stateLock.Lock()
		delete(self.listeners, listener)
		self.stateLock.Unlock()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if self.ctx.Err() != nil {
				return nil
			}
			return err
		}
		self.stateLock.Lock()
		if self.ctx.Err() != nil {
			self.stateLock.Unlock()
			conn.Close()
			return nil
		}
		self.conns[conn] = true
		self.sessions.Add(1)
		self.stateLock.Unlock()
		go func() {
			defer self.sessions.Done()
			self.serveSession(conn)
		}()
	}
}

// Stops accepting, ends each open session with 421, and returns once every session has closed.
func (self *SmtpSink) Close() {
	self.cancel()
	self.stateLock.Lock()
	for listener := range self.listeners {
		listener.Close()
	}
	for conn := range self.conns {
		// wakes a session waiting on its client; the session answers 421 and closes
		conn.SetReadDeadline(time.Now())
	}
	self.stateLock.Unlock()
	self.sessions.Wait()
}

// The messages kept, oldest first.
func (self *SmtpSink) Messages() []*SmtpSinkMessage {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*SmtpSinkMessage{}, self.messages...)
}

func (self *SmtpSink) status() *smtpSinkStatus {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return &smtpSinkStatus{MessageCount: len(self.messages), DroppedCount: self.droppedCount}
}

func (self *SmtpSink) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.messages = nil
}

func (self *SmtpSink) keep(message *SmtpSinkMessage) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.messageIndex += 1
	message.Index = self.messageIndex
	self.messages = append(self.messages, message)
	if excess := len(self.messages) - self.settings.MessageMaxCount; 0 < excess {
		self.messages = append([]*SmtpSinkMessage{}, self.messages[excess:]...)
		self.droppedCount += excess
	}
}

// The control port's routes.
func (self *SmtpSink) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	writeJson := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJson(w, self.status())
	})
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, r *http.Request) {
		writeJson(w, self.Messages())
	})
	mux.HandleFunc("DELETE /messages", func(w http.ResponseWriter, r *http.Request) {
		self.clear()
		writeJson(w, self.status())
	})
	return mux
}

// One session's reader and writer over its current transport, which STARTTLS replaces.
type smtpSinkSession struct {
	conn     net.Conn
	buffered *bufio.Reader
	// reads DATA's dot-stuffed lines from buffered
	reader *textproto.Reader
	writer *bufio.Writer
}

func newSmtpSinkSession(conn net.Conn, settings *SmtpSinkSettings) *smtpSinkSession {
	// the buffer's size bounds a command line: a longer one fails the read with bufio.ErrBufferFull
	buffered := bufio.NewReaderSize(conn, settings.LineMaxBytes)
	return &smtpSinkSession{
		conn:     conn,
		buffered: buffered,
		reader:   textproto.NewReader(buffered),
		writer:   bufio.NewWriter(conn),
	}
}

// The next command line, without its line end.
func (self *smtpSinkSession) readLine() (string, error) {
	line, err := self.buffered.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func (self *smtpSinkSession) reply(format string, args ...any) error {
	if _, err := fmt.Fprintf(self.writer, format+"\r\n", args...); err != nil {
		return err
	}
	return self.writer.Flush()
}

// Arms the session's next read: within the sink's command timeout, and not at all once the sink is closing.
func (self *SmtpSink) armRead(session *smtpSinkSession) {
	session.conn.SetDeadline(time.Now().Add(self.settings.CommandTimeout))
	if self.ctx.Err() != nil {
		session.conn.SetReadDeadline(time.Now())
	}
}

// Ends a session from the sink's side: its closing answer, then the write side closed, then whatever the
// client still sends discarded until it hangs up or the drain timeout passes, so the answer is read ahead of
// the close and not lost to a reset.
func (self *SmtpSink) endSession(session *smtpSinkSession, format string, args ...any) {
	session.conn.SetDeadline(time.Now().Add(self.settings.CloseDrainTimeout))
	if session.reply(format, args...) != nil {
		return
	}
	if closeWriter, ok := session.conn.(interface{ CloseWrite() error }); ok {
		closeWriter.CloseWrite()
	}
	io.Copy(io.Discard, session.conn)
}

// Ends a session whose read or write failed: with 421 when the sink is closing, with 500 for a command line
// past the limit, and with nothing for a client that went away or sat past the command timeout.
func (self *SmtpSink) endFailedSession(session *smtpSinkSession, err error) {
	if self.ctx.Err() != nil {
		self.endSession(session, "421 4.3.2 %s closing", self.settings.Hostname)
	} else if errors.Is(err, bufio.ErrBufferFull) {
		self.endSession(session, "500 5.5.2 the line is past %d bytes", self.settings.LineMaxBytes)
	}
}

func (self *SmtpSink) serveSession(conn net.Conn) {
	remoteAddress := conn.RemoteAddr().String()
	session := newSmtpSinkSession(conn, self.settings)
	defer func() {
		session.conn.Close()
		self.stateLock.Lock()
		delete(self.conns, conn)
		delete(self.conns, session.conn)
		self.stateLock.Unlock()
	}()

	helo := ""
	usesTls := false
	var from *string
	recipients := []string{}
	reset := func() {
		from = nil
		recipients = []string{}
	}

	if session.reply("220 %s ESMTP sink", self.settings.Hostname) != nil {
		return
	}
	for {
		self.armRead(session)
		line, err := session.readLine()
		if err != nil {
			self.endFailedSession(session, err)
			return
		}
		verb, argument, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		argument = strings.TrimSpace(argument)
		switch verb {
		case "EHLO", "HELO":
			if argument == "" {
				err = session.reply("501 5.5.4 a name is required")
				break
			}
			helo = argument
			reset()
			if verb == "HELO" {
				err = session.reply("250 %s", self.settings.Hostname)
				break
			}
			lines := []string{self.settings.Hostname, fmt.Sprintf("SIZE %d", self.settings.MessageMaxBytes), "8BITMIME", "SMTPUTF8", "ENHANCEDSTATUSCODES"}
			if self.tlsConfig != nil && !usesTls {
				lines = append(lines, "STARTTLS")
			}
			for index, extension := range lines {
				separator := "-"
				if index == len(lines)-1 {
					separator = " "
				}
				fmt.Fprintf(session.writer, "250%s%s\r\n", separator, extension)
			}
			err = session.writer.Flush()
		case "STARTTLS":
			if self.tlsConfig == nil || usesTls {
				err = session.reply("502 5.5.1 STARTTLS is not offered")
				break
			}
			if 0 < session.buffered.Buffered() {
				// bytes sent ahead of the handshake would be read as if they came under TLS
				err = session.reply("503 5.5.1 nothing may follow STARTTLS in the clear")
				break
			}
			if err = session.reply("220 2.0.0 ready to start TLS"); err != nil {
				break
			}
			tlsConn := tls.Server(session.conn, self.tlsConfig)
			self.stateLock.Lock()
			self.conns[tlsConn] = true
			self.stateLock.Unlock()
			session = newSmtpSinkSession(tlsConn, self.settings)
			self.armRead(session)
			if err = tlsConn.HandshakeContext(self.ctx); err != nil {
				break
			}
			usesTls = true
			// a session starts over under TLS (RFC 3207 4.2)
			helo = ""
			reset()
		case "MAIL":
			path, ok := smtpSinkPath(argument, "FROM:")
			if !ok {
				err = session.reply("501 5.5.4 MAIL FROM:<address>")
			} else if helo == "" {
				err = session.reply("503 5.5.1 EHLO first")
			} else if from != nil {
				err = session.reply("503 5.5.1 a transaction is open")
			} else {
				from = &path
				err = session.reply("250 2.1.0 ok")
			}
		case "RCPT":
			path, ok := smtpSinkPath(argument, "TO:")
			if !ok || path == "" {
				err = session.reply("501 5.5.4 RCPT TO:<address>")
			} else if from == nil {
				err = session.reply("503 5.5.1 MAIL first")
			} else if self.settings.RecipientMaxCount <= len(recipients) {
				err = session.reply("452 4.5.3 too many recipients")
			} else {
				recipients = append(recipients, path)
				err = session.reply("250 2.1.5 ok")
			}
		case "DATA":
			if from == nil || len(recipients) == 0 {
				err = session.reply("503 5.5.1 RCPT first")
				break
			}
			if err = session.reply("354 end with <CRLF>.<CRLF>"); err != nil {
				break
			}
			self.armRead(session)
			dotReader := session.reader.DotReader()
			var data []byte
			data, err = io.ReadAll(io.LimitReader(dotReader, int64(self.settings.MessageMaxBytes)+1))
			if err != nil {
				break
			}
			if self.settings.MessageMaxBytes < len(data) {
				// a message past the limit is read to its end, up to as much again, and refused; one
				// longer still closes the session
				if _, err = io.CopyN(io.Discard, dotReader, int64(self.settings.MessageMaxBytes)); err != io.EOF {
					if err == nil {
						err = fmt.Errorf("the message is past twice %d bytes", self.settings.MessageMaxBytes)
					}
					break
				}
				reset()
				err = session.reply("552 5.3.4 the message is past %d bytes", self.settings.MessageMaxBytes)
				break
			}
			message := &SmtpSinkMessage{
				RemoteAddress: remoteAddress,
				Helo:          helo,
				Tls:           usesTls,
				From:          *from,
				Recipients:    recipients,
				Data:          data,
				ReceivedTime:  time.Now().UTC(),
			}
			reset()
			self.keep(message)
			err = session.reply("250 2.0.0 taken as %d", message.Index)
		case "RSET":
			reset()
			err = session.reply("250 2.0.0 ok")
		case "NOOP":
			err = session.reply("250 2.0.0 ok")
		case "QUIT":
			session.reply("221 2.0.0 %s closing", self.settings.Hostname)
			return
		default:
			err = session.reply("502 5.5.1 not a command here")
		}
		if err != nil {
			self.endFailedSession(session, err)
			return
		}
	}
}

// The path of a MAIL or RCPT argument, `FROM:<a@b> SIZE=1` giving a@b; the null path <> gives "".
func smtpSinkPath(argument string, prefix string) (string, bool) {
	if len(argument) < len(prefix) || !strings.EqualFold(argument[:len(prefix)], prefix) {
		return "", false
	}
	rest := strings.TrimSpace(argument[len(prefix):])
	if !strings.HasPrefix(rest, "<") {
		return "", false
	}
	end := strings.Index(rest, ">")
	if end < 0 {
		return "", false
	}
	return rest[1:end], true
}

func main() {
	settings := DefaultSmtpSinkSettings()
	smtpAddress := flag.String("smtp", ":2525", "the address SMTP is served on")
	controlAddress := flag.String("control", ":8025", "the address the control port is served on")
	startTls := flag.Bool("starttls", true, "offer STARTTLS on a throwaway certificate")
	flag.StringVar(&settings.Hostname, "hostname", settings.Hostname, "the name the sink greets as")
	flag.IntVar(&settings.MessageMaxBytes, "message-max-bytes", settings.MessageMaxBytes, "the largest message taken")
	flag.IntVar(&settings.MessageMaxCount, "message-max-count", settings.MessageMaxCount, "how many messages are kept")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	sink, err := NewSmtpSink(ctx, settings, *startTls)
	if err != nil {
		log.Fatal(err)
	}
	smtpListener, err := net.Listen("tcp", *smtpAddress)
	if err != nil {
		log.Fatal(err)
	}
	controlListener, err := net.Listen("tcp", *controlAddress)
	if err != nil {
		log.Fatal(err)
	}
	controlServer := &http.Server{Handler: sink.ControlHandler(), ReadHeaderTimeout: settings.CommandTimeout}

	failed := make(chan error, 2)
	go func() {
		if err := sink.ServeSmtp(smtpListener); err != nil {
			failed <- err
		}
	}()
	go func() {
		if err := controlServer.Serve(controlListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()
	log.Printf("smtpsink: smtp on %s, control on %s, as %s", smtpListener.Addr(), controlListener.Addr(), settings.Hostname)

	code := 0
	select {
	case <-ctx.Done():
	case err := <-failed:
		log.Printf("smtpsink: %v", err)
		code = 1
	}
	sink.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), settings.CommandTimeout)
	defer cancel()
	controlServer.Shutdown(shutdownCtx)
	log.Printf("smtpsink: stopped with %d messages kept", sink.status().MessageCount)
	os.Exit(code)
}
