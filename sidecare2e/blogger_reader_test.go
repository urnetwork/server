//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The blogger case's readers (blogger_test.go): visitors on the internet, each at a documentation address of its own
// behind the world's lb, loading a blog's articles through tlsedge's 443, one TLS connection a request, so each request
// is one Find and sockets' scheduler picks the writer that answers it (§9.7):
//
//	visit     World.DialLb to tlsedge behind the lb's PROXY v1 header naming the reader, TLS 1.3 for the blog's
//	          server name offering the readers' ALPN ids, the leaf held to one of the blog's writers' keys, then one
//	          HTTP/1.1 GET /articles/<id> carrying the newest version of it the reader was answered (X-Blog-Seen),
//	          with Connection: close
//	verify    a 200's body must hold the article's network, id and writer, a version and the digest of its lines, its
//	          X-Blog-Writer must name the writer whose key the leaf was on, and its version must not be below the
//	          reader's newest
//	capture   every byte tlsedge took from the visitor past the lb's header, and every byte it gave back, is added to
//	          the world's privacy capture under CC1's handler class, tlsedge's input and output, and each direction
//	          is read as TLS records: past its hello, every record must be encrypted
//
// A visit that fails below HTTP (no dial, no handshake, a connection cut) is the reader's to try again; one answered
// with anything a writer should not answer is a problem, never tried again. A reader is one burst's: its seen versions
// are its own goroutine's, behind a lock only so a dump may read them.

package sidecare2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
)

// TLS's record content types (RFC 8446 §5.1) and handshake message types (§4).
const (
	bloggerTlsRecordChangeCipherSpec byte = 20
	bloggerTlsRecordAlert            byte = 21
	bloggerTlsRecordHandshake        byte = 22
	bloggerTlsRecordApplicationData  byte = 23
	bloggerTlsClientHello            byte = 1
	bloggerTlsServerHello            byte = 2
	bloggerTlsRecordHeaderByteCount       = 5
)

// One reader of a blog. See the file header.
type bloggerReader struct {
	blog    *bloggerBlog
	index   int
	name    string
	address netip.Addr

	stateLock sync.Mutex
	// the newest version of each article this reader was answered, by id
	articleIdSeenVersions map[string]int
	// the source port its next visit's PROXY header names
	nextPort int
}

// The newest version of articleId this reader was answered, 0 for none.
func (self *bloggerReader) seenVersion(articleId string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.articleIdSeenVersions[articleId]
}

// Keeps version as articleId's newest seen, if it is.
func (self *bloggerReader) noteSeen(articleId string, version int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.articleIdSeenVersions[articleId] < version {
		self.articleIdSeenVersions[articleId] = version
	}
}

// The address the next visit comes from, as the lb's PROXY header names it: the reader's own, at the next port of
// its range.
func (self *bloggerReader) nextSource() netip.AddrPort {
	settings := self.blog.settings
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	port := settings.VisitorPortStart + self.nextPort%settings.VisitorPortCount
	self.nextPort += 1
	return netip.AddrPortFrom(self.address, uint16(port))
}

// One visit as its reader saw it.
type bloggerVisit struct {
	reader    *bloggerReader
	phase     string
	articleId string
	// the version the reader sent as the newest it had seen
	seen      int
	startedAt time.Time
	endedAt   time.Time
	// the writer whose key the leaf was on, nil before a handshake completed
	writer *bloggerWriter
	// the answer: its status, the run that gave it and where its body came from, what the run's cache held of the
	// article, -1 for nothing, and the body's fields once they check
	status        int
	writerName    string
	run           int
	source        string
	cachedVersion int
	fields        *bloggerArticleFields
	// a failure below HTTP, which the reader tries again
	err error
	// an answer a writer should not give, which nothing tries again
	problem error
	// the bytes tlsedge took past the lb's header, and gave back
	sentByteCount     int
	receivedByteCount int
	// whether the visitor's ClientHello named the server, and what either direction showed in the clear past its
	// hello, "" for nothing
	helloNamesServer bool
	plaintext        string
}

// Whether the visit completed TLS with one of the blog's writers, so tlsedge spliced it.
func (self *bloggerVisit) spliced() bool {
	return self.writer != nil
}

// One line of the visit, for a failure's message.
func (self *bloggerVisit) describe() string {
	line := fmt.Sprintf("%s's %s visit of %s at %s (seen %d)", self.reader.name, self.phase, self.articleId, self.startedAt.Format("15:04:05.000"), self.seen)
	if self.err != nil {
		return line + fmt.Sprintf(": failed, %v", self.err)
	}
	line += fmt.Sprintf(": %d from %s run %d, %s, cache held %d", self.status, self.writerName, self.run, self.source, self.cachedVersion)
	if self.fields != nil {
		line += fmt.Sprintf(", version %d", self.fields.version)
	}
	if self.problem != nil {
		line += fmt.Sprintf(", %v", self.problem)
	}
	return line
}

// One visit to articleId: see the file header. Never nil.
func (self *bloggerReader) visit(ctx context.Context, phase string, articleId string) *bloggerVisit {
	blog := self.blog
	settings := blog.settings
	world := blog.world
	visit := &bloggerVisit{
		reader:        self,
		phase:         phase,
		articleId:     articleId,
		seen:          self.seenVersion(articleId),
		startedAt:     time.Now(),
		cachedVersion: -1,
	}
	ctx, cancel := context.WithTimeout(ctx, settings.VisitTimeout)
	defer cancel()
	conn, err := world.DialLb(ctx, self.nextSource(), netip.AddrPortFrom(world.Settings().LbAddress, worldTlsPort))
	if err != nil {
		visit.err = fmt.Errorf("the dial: %w", err)
		visit.endedAt = time.Now()
		return visit
	}
	recording := &bloggerRecordingConn{Conn: conn}
	// the client's close first, its close_notify recorded, then what the edge took and gave
	defer func() {
		recording.Close()
		blog.noteVisitBytes(visit, recording)
		visit.endedAt = time.Now()
	}()
	if deadline, ok := ctx.Deadline(); ok {
		recording.SetDeadline(deadline)
	}
	client := tls.Client(recording, &tls.Config{
		ServerName: blog.serverName,
		MinVersion: tls.VersionTLS13,
		NextProtos: slices.Clone(settings.Alpns),
		// the writers' leaves are self-signed on their own keys, pinned below
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return errors.New("no certificate")
			}
			spki, err := x509.MarshalPKIXPublicKey(state.PeerCertificates[0].PublicKey)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(spki)
			writer := blog.writerOfSpki(digest[:])
			if writer == nil {
				return fmt.Errorf("the leaf is on a key no writer of %s holds: %x", blog.network.Name, digest)
			}
			visit.writer = writer
			return nil
		},
	})
	defer client.Close()
	if err := client.HandshakeContext(ctx); err != nil {
		visit.err = fmt.Errorf("the handshake: %w", err)
		return visit
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+blog.serverName+bloggerArticlePathPrefix+articleId, nil)
	if err != nil {
		visit.problem = err
		return visit
	}
	request.Close = true
	if 0 < visit.seen {
		request.Header.Set(bloggerHeaderSeen, strconv.Itoa(visit.seen))
	}
	if err := request.Write(client); err != nil {
		visit.err = fmt.Errorf("the request: %w", err)
		return visit
	}
	response, err := http.ReadResponse(bufio.NewReaderSize(client, settings.BodyMaxByteCount), request)
	if err != nil {
		visit.err = fmt.Errorf("the answer: %w", err)
		return visit
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(settings.BodyMaxByteCount)+1))
	response.Body.Close()
	if err != nil {
		visit.err = fmt.Errorf("the answer's body: %w", err)
		return visit
	}
	visit.status = response.StatusCode
	visit.writerName = response.Header.Get(bloggerHeaderWriter)
	visit.run, _ = strconv.Atoi(response.Header.Get(bloggerHeaderRun))
	visit.source = response.Header.Get(bloggerHeaderSource)
	if text := response.Header.Get(bloggerHeaderCachedVersion); text != "" {
		if visit.cachedVersion, err = strconv.Atoi(text); err != nil {
			visit.problem = fmt.Errorf("a cached version %q", text)
			return visit
		}
	}
	if visit.writerName != visit.writer.device.Name {
		visit.problem = fmt.Errorf("the answer names writer %q, and the leaf was %s's", visit.writerName, visit.writer.device.Name)
		return visit
	}
	switch visit.status {
	case http.StatusOK:
		visit.problem = self.verify(visit, body)
	case http.StatusNotFound:
		// held to the visit's kind by its caller: a read of an article the blog holds is never answered 404
	default:
		visit.problem = fmt.Errorf("answered %d: %q", visit.status, body)
	}
	return visit
}

// Holds a 200's body to the article the visit asked for (see the file header), and keeps its version as seen.
func (self *bloggerReader) verify(visit *bloggerVisit, body []byte) error {
	blog := self.blog
	if blog.settings.BodyMaxByteCount < len(body) {
		return fmt.Errorf("a body past %d bytes", blog.settings.BodyMaxByteCount)
	}
	fields, err := parseBloggerArticleBody(body)
	if err != nil {
		return err
	}
	visit.fields = fields
	article := blog.articleIdArticles[visit.articleId]
	switch {
	case article == nil:
		return fmt.Errorf("an article %s does not hold was answered", blog.network.Name)
	case fields.network != blog.network.Name:
		return fmt.Errorf("the body is of network %s, not %s", fields.network, blog.network.Name)
	case fields.articleId != visit.articleId:
		return fmt.Errorf("the body is article %s", fields.articleId)
	case fields.writer != article.writer.device.Name:
		return fmt.Errorf("the body names writer %s, and %s writes the article", fields.writer, article.writer.device.Name)
	case visit.source != bloggerSourceCache && visit.source != bloggerSourceEvs:
		return fmt.Errorf("an answer from %q", visit.source)
	case fields.version < visit.seen:
		return fmt.Errorf("version %d, below the %d this reader was answered before: the article went backwards", fields.version, visit.seen)
	}
	self.noteSeen(visit.articleId, fields.version)
	return nil
}

// Keeps what the edge took and gave in a visit: its byte counts, both directions in the world's capture as tlsedge's
// input and output, the hello's server name and what either direction showed in the clear.
func (self *bloggerBlog) noteVisitBytes(visit *bloggerVisit, recording *bloggerRecordingConn) {
	sent, received := recording.recorded()
	visit.sentByteCount = len(sent)
	visit.receivedByteCount = len(received)
	capture := self.world.T3().Net().Capture()
	name := fmt.Sprintf("%s of %s's visit at %s", bloggerTlsedgeSource, visit.reader.name, visit.startedAt.Format(time.RFC3339Nano))
	capture.Add(sidecartest.PrivacyClassHandler, name+": its input", sent)
	capture.Add(sidecartest.PrivacyClassHandler, name+": its output", received)
	visit.helloNamesServer = bloggerHelloNames(sent, self.serverName)
	if plaintext := bloggerTlsPlaintext(sent, bloggerTlsClientHello); plaintext != "" {
		visit.plaintext = "the visitor's bytes: " + plaintext
	} else if plaintext := bloggerTlsPlaintext(received, bloggerTlsServerHello); plaintext != "" {
		visit.plaintext = "the edge's bytes: " + plaintext
	}
}

// The source names the case's capture items begin with.
const bloggerTlsedgeSource = "blogger tlsedge"

// Whether stream's first record is a handshake record holding a ClientHello that names serverName.
func bloggerHelloNames(stream []byte, serverName string) bool {
	if len(stream) < bloggerTlsRecordHeaderByteCount || stream[0] != bloggerTlsRecordHandshake {
		return false
	}
	end := bloggerTlsRecordHeaderByteCount + int(binary.BigEndian.Uint16(stream[3:5]))
	if len(stream) < end {
		return false
	}
	body := stream[bloggerTlsRecordHeaderByteCount:end]
	return 0 < len(body) && body[0] == bloggerTlsClientHello && bytes.Contains(body, []byte(serverName))
}

// What one direction of a TLS 1.3 connection showed in the clear past its hello: "" when every record after the
// hello's was application data or a compatibility change_cipher_spec, an alert being allowed before the first
// encrypted record, since a refusal carries only its code; else what it showed. helloType is the one handshake
// message the direction sends in the clear: a ClientHello, or a ServerHello, a HelloRetryRequest being one too. A
// record the connection's end cut short is not read.
func bloggerTlsPlaintext(stream []byte, helloType byte) string {
	encrypted := false
	for offset := 0; offset+bloggerTlsRecordHeaderByteCount <= len(stream); {
		contentType := stream[offset]
		end := offset + bloggerTlsRecordHeaderByteCount + int(binary.BigEndian.Uint16(stream[offset+3:offset+5]))
		if len(stream) < end {
			return ""
		}
		body := stream[offset+bloggerTlsRecordHeaderByteCount : end]
		switch contentType {
		case bloggerTlsRecordHandshake:
			if encrypted {
				return fmt.Sprintf("a handshake record in the clear after an encrypted one, at byte %d", offset)
			}
			if len(body) == 0 || body[0] != helloType {
				return fmt.Sprintf("a handshake record in the clear at byte %d that is not the direction's hello", offset)
			}
		case bloggerTlsRecordChangeCipherSpec:
		case bloggerTlsRecordAlert:
			if encrypted {
				return fmt.Sprintf("an alert in the clear after an encrypted record, at byte %d", offset)
			}
		case bloggerTlsRecordApplicationData:
			encrypted = true
		default:
			return fmt.Sprintf("a record of type %d at byte %d", contentType, offset)
		}
		offset = end
	}
	return ""
}

// A connection that keeps a copy of what was written to it and read from it.
type bloggerRecordingConn struct {
	net.Conn

	stateLock sync.Mutex
	sent      bytes.Buffer
	received  bytes.Buffer
}

func (self *bloggerRecordingConn) Read(buffer []byte) (int, error) {
	n, err := self.Conn.Read(buffer)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.received.Write(buffer[:n])
	return n, err
}

func (self *bloggerRecordingConn) Write(data []byte) (int, error) {
	n, err := self.Conn.Write(data)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sent.Write(data[:n])
	return n, err
}

// Copies of what was written and what was read.
func (self *bloggerRecordingConn) recorded() ([]byte, []byte) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return bytes.Clone(self.sent.Bytes()), bytes.Clone(self.received.Bytes())
}
