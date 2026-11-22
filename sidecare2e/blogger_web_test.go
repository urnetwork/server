//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The blogger case's writers (blogger_test.go): each a sidecar of a blog's network that writes its own articles as
// files in the blog's one repo on evs and runs an HTTPS webserver behind tlsedge on the blog's one name, serving
// GET /articles/<id> from a cache of its own and reading through evs on a miss.
//
//	listen    each run of the webserver listens raw on tls/443/<host> through the sidecar's socket component
//	          (Sidecar.Listen, through World.Listen), beside every other writer of the network, so sockets' fair
//	          scheduler picks one of them for each visit tlsedge Finds (§9.7, R-SOCK-2)
//	tls       TLS ends in the writer's own process on its device's TLS key, the self-signed leaf the world's
//	          certificate path gives (World.IssueTlsCertificateRow, Sidecar.TlsDeviceCertificate): the world runs no
//	          CA, so a reader pins the writers' keys
//	cache     one entry per article id, the newest body this run read: a hit serves it unless the reader has seen a
//	          newer version (X-Blog-Seen), which a stale entry cannot give and which is read through instead; a miss
//	          reads the path through evs, fresh (§11.9), and keeps it unless the cache was cleared while the read ran,
//	          so a clear is never undone by a read begun before it
//	changes   the cache is cleared at every change of the blog repo the writer's EvsClient raises: the run listens
//	          with EvsClient.AddRepoChangeListener over the subscription EvsRepo.Subscribe leased for the article
//	          directory, and evs's hint that the repo's ref moved (§11.10's hint, §6.8's PushHint on evs:<repo_id>)
//	          reaches the client through its evs component (NewEvsComponent on Sidecar.AddComponent), whose
//	          subscriber reads the ref and raises a change for each statement its read brings. The writer's own write
//	          of an article drops that article's entry too, since the writer knows of it without a hint
//	runs      chaos stops a writer's webserver, its listener unlistened, its server and cache gone, and starts a new
//	          run on a cold cache; the sidecar itself runs on, since the world restarts no device
//
// Every answer names its writer, its run, where its body came from and what the cache held for the article, inside
// TLS, so the case can hold each read to the commits it knows of. Every cache clear and every rise of the writer's
// pin that its own reads and writes made are kept, the evidence a missed notification is reported with. Safe for
// concurrent use.

package sidecare2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/sidecart3"
)

// The answer's headers a writer sets and a reader reads, and the request header a reader sends: all inside TLS.
const (
	bloggerHeaderWriter        = "X-Blog-Writer"
	bloggerHeaderRun           = "X-Blog-Run"
	bloggerHeaderSource        = "X-Blog-Source"
	bloggerHeaderCachedVersion = "X-Blog-Cached-Version"
	bloggerHeaderSeen          = "X-Blog-Seen"
)

// Where an answer's body came from.
const (
	bloggerSourceCache = "cache"
	bloggerSourceEvs   = "evs"
)

// The request path an article is read at, before its id.
const bloggerArticlePathPrefix = "/articles/"

// Why a run's cache was cleared, or an entry dropped.
const (
	bloggerClearChange   = "change"
	bloggerClearOwnWrite = "own write"
)

// Why a writer's pin rose under a call of its own.
const (
	bloggerPinRiseWrite = "write"
	bloggerPinRiseRead  = "read"
)

// Where a writer stands: stopped, its writes waiting; up, a run of its webserver serving; or abandoned once a start
// failed, its writes ended.
const (
	bloggerWriterStopped = iota
	bloggerWriterUp
	bloggerWriterAbandoned
)

// One article of a blog: its writer, its id, which carries a canary of the world's capture, and its path in the repo.
type bloggerArticle struct {
	blog   *bloggerBlog
	writer *bloggerWriter
	id     string
	path   string
}

// The body of article at version: its network, id, version and writer, text, a canary of the world's capture, and
// the SHA-256 of the lines before it.
func bloggerArticleBody(article *bloggerArticle, version int, text string) []byte {
	head := strings.Join([]string{
		"blogger article",
		"network: " + article.blog.network.Name,
		"article: " + article.id,
		"version: " + strconv.Itoa(version),
		"writer: " + article.writer.device.Name,
		"text: " + text,
	}, "\n") + "\n"
	digest := sha256.Sum256([]byte(head))
	return []byte(head + "digest: " + hex.EncodeToString(digest[:]) + "\n")
}

// What an article's body says, its digest checked.
type bloggerArticleFields struct {
	network   string
	articleId string
	version   int
	writer    string
	text      string
}

// Reads body as bloggerArticleBody writes it, refusing one whose digest is not the SHA-256 of its lines before it.
func parseBloggerArticleBody(body []byte) (*bloggerArticleFields, error) {
	head, digestLine, found := strings.Cut(string(body), "digest: ")
	if !found {
		return nil, errors.New("a body with no digest")
	}
	digest := sha256.Sum256([]byte(head))
	if strings.TrimSuffix(digestLine, "\n") != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("a body whose digest %q is not its lines' %x", strings.TrimSuffix(digestLine, "\n"), digest)
	}
	lines := strings.Split(strings.TrimSuffix(head, "\n"), "\n")
	if len(lines) != 6 || lines[0] != "blogger article" {
		return nil, fmt.Errorf("a body of %d lines, not an article's", len(lines))
	}
	fieldValues := map[string]string{}
	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ": ")
		if !found {
			return nil, fmt.Errorf("a body line %q", line)
		}
		fieldValues[name] = value
	}
	version, err := strconv.Atoi(fieldValues["version"])
	if err != nil || version < 1 {
		return nil, fmt.Errorf("a body's version %q", fieldValues["version"])
	}
	return &bloggerArticleFields{
		network:   fieldValues["network"],
		articleId: fieldValues["article"],
		version:   version,
		writer:    fieldValues["writer"],
		text:      fieldValues["text"],
	}, nil
}

// One writer of a blog: its device, its articles, its webserver's runs and the subscription that hears the blog
// repo's changes. See the file header.
type bloggerWriter struct {
	blog   *bloggerBlog
	device *sidecart3.T3Device
	// its own articles, in order
	articles []*bloggerArticle
	client   *sidecar.EvsClient
	// the leaf its runs serve, on the device's TLS key, and that key's SubjectPublicKeyInfo's SHA-256, which a reader
	// pins the writer by
	certificate  *tls.Certificate
	spkiSha256   []byte
	subscription *sidecar.EvsSubscription
	// where it stands (bloggerWriterStopped and on): chaos's stop holds the writer's writes until its start, and a
	// failed start ends them
	state *sidecar.MonitorValue[int]

	stateLock sync.Mutex
	// the running run, nil while stopped; every run, in order
	server  *bloggerServer
	servers []*bloggerServer
	// each article's newest version written, by id
	articleIdVersions map[string]int
	pinRises          []*bloggerPinRise
}

// A rise of a writer's pin of the blog's main ref across one of its own calls: what it rose from and to, and which call.
type bloggerPinRise struct {
	at        time.Time
	from      uint64
	to        uint64
	cause     string
	articleId string
}

// The writer's pin of the blog's main ref, 0 while it holds none.
func (self *bloggerWriter) pinVersion() uint64 {
	pin, ok, err := self.client.Pins().Pin(self.blog.repoId, self.blog.refId)
	if err != nil || !ok {
		return 0
	}
	return pin.Version
}

// Keeps a rise of the pin from before to after across a call of cause on articleId, nothing when it did not rise.
func (self *bloggerWriter) notePinRise(before uint64, after uint64, cause string, articleId string) {
	if after <= before {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pinRises = append(self.pinRises, &bloggerPinRise{at: time.Now(), from: before, to: after, cause: cause, articleId: articleId})
}

// Every pin rise kept, in order.
func (self *bloggerWriter) pinRiseList() []*bloggerPinRise {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.pinRises)
}

// The run that serves now, nil while stopped.
func (self *bloggerWriter) currentServer() *bloggerServer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.server
}

// Every run, in order.
func (self *bloggerWriter) serverList() []*bloggerServer {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.servers)
}

// Waits until a run serves: a stopped writer writes nothing, and an abandoned one never will.
func (self *bloggerWriter) awaitUp(ctx context.Context) error {
	for {
		state, changed := self.state.Get()
		switch state {
		case bloggerWriterUp:
			return nil
		case bloggerWriterAbandoned:
			return fmt.Errorf("%s was abandoned: its webserver did not start again", self.device.Name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s stayed stopped: %w", self.device.Name, ctx.Err())
		case <-changed:
		}
	}
}

// Ends the writer's writes for good: its restart failed.
func (self *bloggerWriter) abandon() {
	self.state.Set(bloggerWriterAbandoned)
}

// Writes article's next version through the writer's EvsClient, as its sidecar writes any file (EvsRepo.Write), and
// keeps the commit in the blog's ledger once evs answered it. The running webserver's entry for the article is dropped
// then: the writer knows of its own write without a hint. text is the body's canary.
func (self *bloggerWriter) write(ctx context.Context, article *bloggerArticle, text string) (*bloggerCommit, error) {
	version := func() int {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.articleIdVersions[article.id] + 1
	}()
	body := bloggerArticleBody(article, version, text)
	before := self.pinVersion()
	startedAt := time.Now()
	result, err := self.client.Repo(self.blog.repoId).Write(ctx, &sidecar.EvsWriteArgs{
		Path:    article.path,
		Content: bytes.NewReader(body),
		Size:    int64(len(body)),
	})
	if err != nil {
		return nil, fmt.Errorf("%s's write of %s at version %d: %w", self.device.Name, article.id, version, err)
	}
	answeredAt := time.Now()
	self.notePinRise(before, self.pinVersion(), bloggerPinRiseWrite, article.id)
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.articleIdVersions[article.id] = version
	}()
	if server := self.currentServer(); server != nil {
		server.drop(article.id)
	}
	commit := &bloggerCommit{
		article:     article,
		version:     version,
		pathVersion: result.PathVersion,
		refVersion:  result.Version,
		startedAt:   startedAt,
		answeredAt:  answeredAt,
	}
	self.blog.ledger.addCommit(commit)
	return commit, nil
}

// Starts a run of the writer's webserver on a cold cache: its change listener first, so nothing it caches misses a
// change, then its listen on the blog's name and its HTTPS server over the listener.
func (self *bloggerWriter) start(ctx context.Context) (*bloggerServer, error) {
	blog := self.blog
	settings := blog.settings
	server := &bloggerServer{
		writer:      self,
		startedAt:   time.Now(),
		changeCount: sidecar.NewMonitorValue(0),
		entries:     map[string]*bloggerCacheEntry{},
	}
	server.removeChanges = self.client.AddRepoChangeListener(&blog.repoId, server.noteChange)
	listener, err := blog.listen(ctx, self)
	if err != nil {
		server.removeChanges()
		return nil, err
	}
	server.listener = listener
	config := &tls.Config{
		Certificates: []tls.Certificate{*self.certificate},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   slices.Clone(settings.Alpns),
	}
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: settings.VisitTimeout,
		// a visit cut by chaos is the reader's to report, not the server's log's
		ErrorLog: log.New(io.Discard, "", 0),
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		server.run = len(self.servers) + 1
		self.server = server
		self.servers = append(self.servers, server)
	}()
	server.serving.Go(func() {
		server.http.Serve(tls.NewListener(listener, config))
	})
	self.state.Set(bloggerWriterUp)
	return server, nil
}

// Stops the running run: its listener unlistens at once, its server and open connections close, its change listener
// goes and its cache with it. Nothing while stopped.
func (self *bloggerWriter) stop() {
	server := func() *bloggerServer {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		server := self.server
		self.server = nil
		return server
	}()
	self.state.Update(func(state int) int {
		if state == bloggerWriterUp {
			return bloggerWriterStopped
		}
		return state
	})
	if server != nil {
		server.stop()
	}
}

// One entry of a run's cache: an article's body and its version.
type bloggerCacheEntry struct {
	version int
	body    []byte
}

// One clear of a run's cache, or drop of one entry: when, why, and for a change the version and signer it named.
type bloggerClear struct {
	at              time.Time
	cause           string
	version         uint64
	gap             bool
	signerSidecarId sidecar.Id
	articleId       string
}

// One run of a writer's webserver. See the file header.
type bloggerServer struct {
	writer        *bloggerWriter
	run           int
	startedAt     time.Time
	listener      *sidecar.SocketListener
	http          *http.Server
	serving       sync.WaitGroup
	removeChanges func()
	// the changes it heard, for a wait to follow
	changeCount *sidecar.MonitorValue[int]

	stateLock sync.Mutex
	// by article id
	entries map[string]*bloggerCacheEntry
	// raised at every clear and drop, so a miss's read keeps nothing a clear passed while it ran
	generation uint64
	clears     []*bloggerClear
	stopped    bool
	stoppedAt  time.Time
	// hits, misses, and hits that were older than what the reader had seen and were read through
	hitCount    int
	missCount   int
	bypassCount int
}

// The change listener (EvsClient.AddRepoChangeListener): clears the cache, on the subscriber's goroutine, without
// blocking.
func (self *bloggerServer) noteChange(change *sidecar.EvsRepoChange) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.generation += 1
		clear(self.entries)
		self.clears = append(self.clears, &bloggerClear{
			at:              time.Now(),
			cause:           bloggerClearChange,
			version:         change.Version,
			gap:             change.Gap,
			signerSidecarId: change.SignerSidecarId,
		})
	}()
	self.changeCount.Update(func(count int) int {
		return count + 1
	})
}

// Drops articleId's entry after the writer's own write of it.
func (self *bloggerServer) drop(articleId string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.generation += 1
	delete(self.entries, articleId)
	self.clears = append(self.clears, &bloggerClear{at: time.Now(), cause: bloggerClearOwnWrite, articleId: articleId})
}

// The entry for articleId, nil for none, and the generation it was read at.
func (self *bloggerServer) lookup(articleId string) (*bloggerCacheEntry, uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.entries[articleId], self.generation
}

// Keeps entry for articleId if no clear passed since generation and the cache holds nothing newer.
func (self *bloggerServer) keep(articleId string, entry *bloggerCacheEntry, generation uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.stopped || self.generation != generation {
		return
	}
	if held := self.entries[articleId]; held != nil && entry.version <= held.version {
		return
	}
	self.entries[articleId] = entry
}

// Every clear and drop so far, in order.
func (self *bloggerServer) clearList() []*bloggerClear {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.clears)
}

// The cache's entries now, by article id.
func (self *bloggerServer) entrySnapshot() map[string]*bloggerCacheEntry {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entries := map[string]*bloggerCacheEntry{}
	for articleId, entry := range self.entries {
		entries[articleId] = entry
	}
	return entries
}

// When the run stopped, zero while it serves.
func (self *bloggerServer) stopTime() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.stoppedAt
}

// One line of where the run stands, for a progress dump.
func (self *bloggerServer) progress() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return fmt.Sprintf("%s run %d: %d cached, %d hits, %d misses, %d read through past a reader's version, %d clears, stopped %t", self.writer.device.Name, self.run, len(self.entries), self.hitCount, self.missCount, self.bypassCount, len(self.clears), self.stopped)
}

// Reads articleId's path through evs, fresh, as a miss does: its body and version, ErrEvsPathAbsent's error for an id
// the repo holds no file of.
func (self *bloggerServer) read(ctx context.Context, articleId string) (*bloggerCacheEntry, error) {
	writer := self.writer
	blog := writer.blog
	before := writer.pinVersion()
	var content bytes.Buffer
	_, err := writer.client.Repo(blog.repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: blog.articlePath(articleId), Fresh: true}, &content)
	writer.notePinRise(before, writer.pinVersion(), bloggerPinRiseRead, articleId)
	if err != nil {
		return nil, err
	}
	fields, err := parseBloggerArticleBody(content.Bytes())
	if err != nil {
		return nil, fmt.Errorf("evs answered %s's path with %w", articleId, err)
	}
	return &bloggerCacheEntry{version: fields.version, body: bytes.Clone(content.Bytes())}, nil
}

// Fills the cache with articleId's newest body, as a miss does: what the case warms every run with before the last
// writes, so a run that misses a change shows it.
func (self *bloggerServer) fill(ctx context.Context, articleId string) error {
	_, generation := self.lookup(articleId)
	entry, err := self.read(ctx, articleId)
	if err != nil {
		return err
	}
	self.keep(articleId, entry, generation)
	return nil
}

// GET /articles/<id>: see the file header.
func (self *bloggerServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	settings := self.writer.blog.settings
	articleId, found := strings.CutPrefix(request.URL.Path, bloggerArticlePathPrefix)
	if request.Method != http.MethodGet || !found || !bloggerArticleIdValid(articleId, settings.IdMaxByteCount) {
		http.Error(writer, "no such page", http.StatusNotFound)
		return
	}
	seen := 0
	if text := request.Header.Get(bloggerHeaderSeen); text != "" {
		var err error
		if seen, err = strconv.Atoi(text); err != nil {
			http.Error(writer, "a seen version that is no number", http.StatusBadRequest)
			return
		}
	}
	header := writer.Header()
	header.Set(bloggerHeaderWriter, self.writer.device.Name)
	header.Set(bloggerHeaderRun, strconv.Itoa(self.run))
	header.Set("Content-Type", "text/plain; charset=utf-8")
	entry, generation := self.lookup(articleId)
	if entry != nil {
		header.Set(bloggerHeaderCachedVersion, strconv.Itoa(entry.version))
	}
	if entry != nil && seen <= entry.version {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.hitCount += 1
		}()
		header.Set(bloggerHeaderSource, bloggerSourceCache)
		writer.Write(entry.body)
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.missCount += 1
		if entry != nil {
			self.bypassCount += 1
		}
	}()
	ctx, cancel := context.WithTimeout(request.Context(), settings.VisitTimeout)
	defer cancel()
	read, err := self.read(ctx, articleId)
	switch {
	case errors.Is(err, sidecar.ErrEvsPathAbsent):
		http.Error(writer, "no such article", http.StatusNotFound)
		return
	case err != nil:
		http.Error(writer, fmt.Sprintf("evs: %v", err), http.StatusServiceUnavailable)
		return
	}
	self.keep(articleId, read, generation)
	header.Set(bloggerHeaderSource, bloggerSourceEvs)
	writer.Write(read.body)
}

// Ends the run: see bloggerWriter.stop.
func (self *bloggerServer) stop() {
	self.listener.Close()
	self.http.Close()
	self.serving.Wait()
	self.removeChanges()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.stopped = true
	self.stoppedAt = time.Now()
	clear(self.entries)
}

// Whether id is one an article could have: lowercase letters, digits and '-', at most maxByteCount bytes.
func bloggerArticleIdValid(id string, maxByteCount int) bool {
	if id == "" || maxByteCount < len(id) {
		return false
	}
	for _, character := range []byte(id) {
		if !('a' <= character && character <= 'z') && !('0' <= character && character <= '9') && character != '-' {
			return false
		}
	}
	return true
}
