//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The gkm concurrency case's server-side parts (gkm_concurrency_test.go), gathered in one file as the e2e conventions
// ask, since QD-1028's layout move will move what they reach:
//
//   - the recorder at gkm's origin, in front of the world's lb for the gkm block: every claim of a generation
//     (POST /gkm/groups and POST gen/<g>, the GenRotate read and its signature checked), every delivery of a wraps
//     batch with gkm's answer to each item (its KeyDelivery read, its signature checked under the signer's admitted
//     identity key), every ReaderKey a sidecar publishes (checked under its own identity key), every late ask (a
//     wraps read with ?want=), every re-form's GroupForm and ladder, and every 401 or 403 gkm answers, with the
//     bearer's claims read unverified (its log position, scopes and sidecar, never the token), so the case reads
//     what gkm took and refused from the wire, never from gkm's own objects
//   - the cuts at that origin: a sidecar cut off from gkm, each of its requests answered 503 with no gkm body, as the
//     lb answers with no upstream, which its client takes as no answer from gkm (KF7); and a filter that loses the
//     requests it matches the same way, which the case uses to lose the deliveries a holder makes to a device while it
//     is cut off, so the device must ask for them late
//   - the chaos at that origin: armed for the next request it matches, it either lets the block answer that request
//     in full, restarts the block on its stores and loses the answer (502, as an lb whose upstream went away before
//     the answer reached it), or restarts the block with the request held and loses the request (502 too), so a
//     restart lands exactly between a claim or a delivery and its answer, or just before it
//   - taskworker's part: the world runs no taskworker, so §10.6's reconciler, which hints pending to a repo's writers
//     when an entry shrinks or grows its key holders, runs here on a host of its own over the world's clusters and
//     Redis, composed as taskworker composes it (taskworker/gkm_reconciler.go): a gate cache of its own over the
//     world's log, which every sync of the world waits on, a write budget of its own that reports beside every
//     block's, and a hint writer that resolves sidecar ids through api's internal route on the world's gkm bearer,
//     as the world's gkm block's does; no sweep runs, since no task queue is given
//   - the statements: each is filed through api's POST /network/config/delta as sidecart3's FileStatement files it,
//     its header at the filer's verified pin, then synced on the world's gates and on every device the case still
//     runs, never on a revoked or closed one, whose follower may follow nothing more (T3Network.Sync waits on every
//     device with a follower)
//
// Safe for concurrent use.

package sidecare2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/sidecart3"
)

// The kinds of request the recorder reads.
const (
	gkmConcurrencyRequestOther = iota
	gkmConcurrencyRequestCreate
	gkmConcurrencyRequestClaim
	gkmConcurrencyRequestDeliver
	gkmConcurrencyRequestReaderKey
	gkmConcurrencyRequestWraps
	gkmConcurrencyRequestForm
	gkmConcurrencyRequestLadder
	gkmConcurrencyRequestGenerations
)

// One request at gkm's origin, as the recorder reads it.
type gkmConcurrencyRequest struct {
	time   time.Time
	method string
	path   string
	query  url.Values
	// the method and path, its ids and numbers as {id} and {n}
	route string
	kind  int
	// the bearer's claims, read unverified; nil where it holds none that parse
	claims    *evsConcurrencyServiceTokenClaims
	sidecarId sidecar.Id
	// the path's group or repo, and its generation, where it names them
	groupId    sidecar.Id
	repoId     sidecar.Id
	generation uint64
	body       []byte
}

// The sidecar the bearer names, and whether it names one.
func (self *gkmConcurrencyRequest) bearerSidecar() (sidecar.Id, bool) {
	return self.sidecarId, self.sidecarId != (sidecar.Id{})
}

// A late ask: a wraps read that wants a delivery to a key.
func (self *gkmConcurrencyRequest) lateAsk() bool {
	return self.kind == gkmConcurrencyRequestWraps && self.query.Get("want") != ""
}

// Reads r: its route, bearer, ids and body, which is put back for the lb.
func readGkmConcurrencyRequest(r *http.Request) *gkmConcurrencyRequest {
	request := &gkmConcurrencyRequest{
		time:   time.Now(),
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
		route:  r.Method + " " + evsConcurrencyRoute(r.URL.Path),
	}
	if claims := evsConcurrencyBearerClaims(r); claims != nil {
		request.claims = claims
		request.sidecarId, _ = sidecar.ParseId(claims.SidecarId)
	}
	if r.Body != nil && r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err == nil {
			request.body = body
		}
	}
	parseId := func(text string) sidecar.Id {
		raw, err := hex.DecodeString(text)
		if err != nil {
			return sidecar.Id{}
		}
		id, err := sidecar.IdFromBytes(raw)
		if err != nil {
			return sidecar.Id{}
		}
		return id
	}
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// gkm, groups|repos, <id>, rest...
	if len(segments) < 2 || segments[0] != "gkm" {
		return request
	}
	switch {
	case segments[1] == "groups" && len(segments) == 2 && r.Method == http.MethodPost:
		request.kind = gkmConcurrencyRequestCreate
	case segments[1] == "groups" && 4 <= len(segments):
		request.groupId = parseId(segments[2])
		if 4 < len(segments) {
			request.generation, _ = strconv.ParseUint(segments[4], 10, 64)
		}
		switch {
		case segments[3] == "gen" && len(segments) == 5 && r.Method == http.MethodPost:
			request.kind = gkmConcurrencyRequestClaim
		case segments[3] == "gen" && r.Method == http.MethodGet:
			request.kind = gkmConcurrencyRequestGenerations
		case segments[3] == "wraps" && len(segments) == 6 && r.Method == http.MethodPost:
			request.kind = gkmConcurrencyRequestDeliver
		case segments[3] == "wraps" && len(segments) == 5 && r.Method == http.MethodGet:
			request.kind = gkmConcurrencyRequestWraps
		case segments[3] == "ladder" && r.Method == http.MethodPost:
			request.kind = gkmConcurrencyRequestLadder
		}
	case segments[1] == "repos" && 4 <= len(segments):
		request.repoId = parseId(segments[2])
		if segments[3] == "forms" && r.Method == http.MethodPost {
			request.kind = gkmConcurrencyRequestForm
		}
	case segments[1] == "reader-key" && len(segments) == 2 && r.Method == http.MethodPost:
		request.kind = gkmConcurrencyRequestReaderKey
	}
	return request
}

// One claim of a generation gkm was asked to take: a POST gen/<g>, or a creation's generation 1, its GenRotate read.
type gkmConcurrencyClaim struct {
	time     time.Time
	uploader sidecar.Id
	groupId  sidecar.Id
	repoId   sidecar.Id
	// the Signed GenRotate's bytes, and its body and signer read unverified (gkmConcurrencyVerifyClaim checks them),
	// nil where it did not parse, then err says why
	signedBytes []byte
	rotate      *protocol.GenRotate
	signerId    sidecar.Id
	err         error
	// the block's answer, 0 where the request never reached it; lost where the chaos lost that answer
	status int
	lost   bool
	// a creation's generation 1, by POST /gkm/groups
	create bool
}

// Whether gkm took the claim: its 201, or equal bytes answering success.
func (self *gkmConcurrencyClaim) taken() bool {
	return self.status == http.StatusCreated || self.status == http.StatusOK
}

// One delivery of a wraps batch, as the recorder read it and gkm answered it.
type gkmConcurrencyDelivery struct {
	time     time.Time
	uploader sidecar.Id
	// the path's group and generation
	groupId    sidecar.Id
	generation uint64
	// the Signed ReaderWrap, and its KeyDelivery and signer read unverified (gkmConcurrencyVerifyDelivery checks
	// them), nil where it did not parse, then err says why
	signed   *bridge.Signed
	body     *protocol.KeyDelivery
	signerId sidecar.Id
	err      error
	// the recipient's sidecar, from the body
	recipient sidecar.Id
	// gkm's answer to this item, 0 for not tried or a batch whose answer never came
	status int
	lost   bool
}

// One ReaderKey a sidecar published: the Signed record, and the SHA-256 of its X-Wing key read unverified
// (gkmConcurrencyVerifyReaderKey checks it under the sidecar's own identity key).
type gkmConcurrencyReaderKey struct {
	time      time.Time
	sidecarId sidecar.Id
	signed    *bridge.Signed
	keySha256 []byte
	err       error
	status    int
}

// One late ask.
type gkmConcurrencyAsk struct {
	time       time.Time
	sidecarId  sidecar.Id
	groupId    sidecar.Id
	generation uint64
	want       string
	status     int
}

// One re-form's GroupForm or ladder call, as gkm answered it.
type gkmConcurrencyForm struct {
	time     time.Time
	uploader sidecar.Id
	repoId   sidecar.Id
	// the form's group, the group it re-forms and its generation; for a ladder call the group alone
	form   *protocol.GroupForm
	ladder bool
	// a ladder call's GenRotates
	generations []*protocol.GenRotate
	status      int
	lost        bool
}

// One refusal gkm answered: a 401 or 403.
type gkmConcurrencyRefusal struct {
	request *gkmConcurrencyRequest
	status  int
	body    string
	// what the case said of the caller at the answer
	device string
}

// One chaos at the origin: the request it takes, and what it does then.
type gkmConcurrencyChaos struct {
	name  string
	match func(request *gkmConcurrencyRequest) bool
	// forward the request to the block first, then lose the block's answer; else lose the request unsent
	lostAnswer bool
	chaos      func() error
	// closed once a request was taken and the chaos ran; the fields below are set before
	taken    chan struct{}
	request  *gkmConcurrencyRequest
	status   int
	chaosErr error
}

// A filter that loses the requests it matches, answering 503 with no gkm body, as the lb does with no upstream.
type gkmConcurrencyCut struct {
	name  string
	match func(request *gkmConcurrencyRequest) bool
	// how many requests it lost
	count int
}

// gkm's origin with the recorder, the cuts and the chaos in front of the world's lb (the file's header). Safe for
// concurrent use.
type gkmConcurrencyOrigin struct {
	lb http.Handler

	stateLock sync.Mutex
	// what the case says of the caller a refusal names; nil until set
	describe func(request *gkmConcurrencyRequest) string
	cuts     []*gkmConcurrencyCut
	armed    *gkmConcurrencyChaos
	// every request the recorder kept, by kind
	claims     []*gkmConcurrencyClaim
	deliveries []*gkmConcurrencyDelivery
	readerKeys []*gkmConcurrencyReaderKey
	asks       []*gkmConcurrencyAsk
	forms      []*gkmConcurrencyForm
	refusals   []*gkmConcurrencyRefusal
	// how many refusals the case has taken
	refusalsTaken int
	// counts every claim, delivery and form kept, for a wait on a new one
	changes *sidecar.MonitorValue[uint64]
}

// Serves the gkm block's origin through the recorder from now on, in place of the lb the world's first run of the block
// put there.
func installGkmConcurrencyOrigin(world *World) (*gkmConcurrencyOrigin, error) {
	block := world.Block(WorldBlockGkm)
	if block == nil || block.service == "" {
		return nil, errors.New("the world serves no gkm origin")
	}
	origin := &gkmConcurrencyOrigin{lb: &worldLb{block: block}, changes: sidecar.NewMonitorValue(uint64(0))}
	api := world.T3().Api()
	if err := api.ServeService(block.service, nil); err != nil {
		return nil, err
	}
	if err := api.ServeService(block.service, origin); err != nil {
		return nil, err
	}
	return origin, nil
}

// Sets what the recorder asks of a refused caller from now on.
func (self *gkmConcurrencyOrigin) setDescribe(describe func(request *gkmConcurrencyRequest) string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.describe = describe
}

// Adds a cut, until its remove.
func (self *gkmConcurrencyOrigin) cut(name string, match func(request *gkmConcurrencyRequest) bool) (*gkmConcurrencyCut, func()) {
	cut := &gkmConcurrencyCut{name: name, match: match}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.cuts = append(self.cuts, cut)
	return cut, func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.cuts = slices.DeleteFunc(self.cuts, func(held *gkmConcurrencyCut) bool { return held == cut })
	}
}

// How many requests cut has lost.
func (self *gkmConcurrencyOrigin) cutCount(cut *gkmConcurrencyCut) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return cut.count
}

// Arms a chaos for the next request match takes; one armed at a time, a later arm replacing an earlier one not taken.
func (self *gkmConcurrencyOrigin) arm(name string, match func(request *gkmConcurrencyRequest) bool, lostAnswer bool, chaos func() error) *gkmConcurrencyChaos {
	armed := &gkmConcurrencyChaos{name: name, match: match, lostAnswer: lostAnswer, chaos: chaos, taken: make(chan struct{})}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.armed = armed
	return armed
}

// Disarms armed, where no request took it.
func (self *gkmConcurrencyOrigin) disarm(armed *gkmConcurrencyChaos) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.armed == armed {
		self.armed = nil
	}
}

func (self *gkmConcurrencyOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	request := readGkmConcurrencyRequest(r)
	var chaos *gkmConcurrencyChaos
	var cut *gkmConcurrencyCut
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, held := range self.cuts {
			if held.match(request) {
				held.count += 1
				cut = held
				return
			}
		}
		if self.armed != nil && self.armed.match(request) {
			chaos = self.armed
			self.armed = nil
			chaos.request = request
		}
	}()
	if cut != nil {
		self.keep(request, http.StatusServiceUnavailable, nil, false, false)
		http.Error(w, "no upstream", http.StatusServiceUnavailable)
		return
	}
	if chaos != nil && !chaos.lostAnswer {
		// the request is lost unsent, the block restarting under it
		chaos.chaosErr = chaos.chaos()
		close(chaos.taken)
		self.keep(request, 0, nil, true, false)
		http.Error(w, "the upstream went away", http.StatusBadGateway)
		return
	}
	answer := httptest.NewRecorder()
	self.lb.ServeHTTP(answer, r)
	self.keep(request, answer.Code, answer.Body.Bytes(), chaos != nil, true)
	if chaos != nil {
		chaos.status = answer.Code
		chaos.chaosErr = chaos.chaos()
		close(chaos.taken)
		http.Error(w, "the upstream went away", http.StatusBadGateway)
		return
	}
	for key, values := range answer.Header() {
		w.Header()[key] = values
	}
	w.WriteHeader(answer.Code)
	w.Write(answer.Body.Bytes())
}

// Keeps what request was and how the block answered it: status 0 where it never reached the block, lost where the
// chaos lost the answer, reached where the block answered. Bodies are read unverified here, since a signer's identity
// key may not be known yet; the case's checks verify them once every key is.
func (self *gkmConcurrencyOrigin) keep(request *gkmConcurrencyRequest, status int, answer []byte, lost bool, reached bool) {
	self.stateLock.Lock()
	describe := self.describe
	self.stateLock.Unlock()
	if reached && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		refusal := &gkmConcurrencyRefusal{request: request, status: status, body: strings.TrimSpace(string(answer))}
		if describe != nil {
			refusal.device = describe(request)
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.refusals = append(self.refusals, refusal)
		}()
	}
	switch request.kind {
	case gkmConcurrencyRequestCreate:
		create := &protocol.GkmGroupCreateRequest{}
		if err := proto.Unmarshal(request.body, create); err != nil {
			return
		}
		claim := &gkmConcurrencyClaim{time: request.time, uploader: request.sidecarId, status: status, lost: lost, create: true}
		claim.signedBytes, claim.err = proto.MarshalOptions{Deterministic: true}.Marshal(create.GetGenRotate())
		if claim.err == nil {
			claim.rotate, claim.signerId, claim.err = readGkmConcurrencyRotate(claim.signedBytes)
		}
		if claim.rotate != nil {
			claim.groupId, _ = sidecar.IdFromBytes(claim.rotate.GetGroupId())
			claim.repoId, _ = sidecar.IdFromBytes(claim.rotate.GetRepoId())
		}
		self.keepClaim(claim)
	case gkmConcurrencyRequestClaim:
		claim := &gkmConcurrencyClaim{time: request.time, uploader: request.sidecarId, groupId: request.groupId, status: status, lost: lost, signedBytes: request.body}
		claim.rotate, claim.signerId, claim.err = readGkmConcurrencyRotate(request.body)
		if claim.rotate != nil {
			claim.repoId, _ = sidecar.IdFromBytes(claim.rotate.GetRepoId())
		}
		self.keepClaim(claim)
	case gkmConcurrencyRequestDeliver:
		batch := &protocol.GkmWrapBatch{}
		if err := proto.Unmarshal(request.body, batch); err != nil {
			return
		}
		reply := &protocol.GkmWrapBatchReply{}
		if status == http.StatusOK && !lost {
			if err := proto.Unmarshal(answer, reply); err != nil {
				reply = &protocol.GkmWrapBatchReply{}
			}
		}
		deliveries := []*gkmConcurrencyDelivery{}
		for index, signedBytes := range batch.GetDeliveries() {
			kept := &gkmConcurrencyDelivery{time: request.time, uploader: request.sidecarId, groupId: request.groupId, generation: request.generation, lost: lost}
			if index < len(reply.GetResults()) {
				kept.status = int(reply.GetResults()[index].GetStatus())
			}
			kept.signed, kept.body, kept.signerId, kept.err = readGkmConcurrencyDelivery(signedBytes)
			if kept.body != nil {
				kept.recipient, _ = sidecar.IdFromBytes(kept.body.GetRecipient())
			}
			deliveries = append(deliveries, kept)
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.deliveries = append(self.deliveries, deliveries...)
		}()
		self.changes.Update(func(count uint64) uint64 { return count + 1 })
	case gkmConcurrencyRequestReaderKey:
		kept := &gkmConcurrencyReaderKey{time: request.time, sidecarId: request.sidecarId, status: status, signed: &bridge.Signed{}}
		body := &protocol.ReaderKey{}
		if err := proto.Unmarshal(request.body, kept.signed); err != nil {
			kept.err = err
		} else if err := proto.Unmarshal(kept.signed.GetBody(), body); err != nil {
			kept.err = err
		} else {
			keySha256 := sha256.Sum256(body.GetXwingPublicKey())
			kept.keySha256 = keySha256[:]
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readerKeys = append(self.readerKeys, kept)
	case gkmConcurrencyRequestWraps:
		if !request.lateAsk() {
			return
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.asks = append(self.asks, &gkmConcurrencyAsk{
			time: request.time, sidecarId: request.sidecarId, groupId: request.groupId, generation: request.generation,
			want: request.query.Get("want"), status: status,
		})
	case gkmConcurrencyRequestForm:
		reform := &protocol.GkmGroupReformRequest{}
		if err := proto.Unmarshal(request.body, reform); err != nil {
			return
		}
		form := &protocol.GroupForm{}
		if err := proto.Unmarshal(reform.GetForm().GetBody(), form); err != nil {
			return
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.forms = append(self.forms, &gkmConcurrencyForm{time: request.time, uploader: request.sidecarId, repoId: request.repoId, form: form, status: status, lost: lost})
		}()
		self.changes.Update(func(count uint64) uint64 { return count + 1 })
	case gkmConcurrencyRequestLadder:
		ladder := &protocol.GkmLadderRequest{}
		if err := proto.Unmarshal(request.body, ladder); err != nil {
			return
		}
		kept := &gkmConcurrencyForm{time: request.time, uploader: request.sidecarId, ladder: true, status: status, lost: lost}
		kept.form = &protocol.GroupForm{GroupId: request.groupId.Bytes()}
		for _, signed := range ladder.GetGenerations() {
			rotate := &protocol.GenRotate{}
			if err := proto.Unmarshal(signed.GetBody(), rotate); err == nil {
				kept.generations = append(kept.generations, rotate)
			}
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.forms = append(self.forms, kept)
		}()
		self.changes.Update(func(count uint64) uint64 { return count + 1 })
	}
}

// A Signed GenRotate's body and signer, read unverified.
func readGkmConcurrencyRotate(signedBytes []byte) (*protocol.GenRotate, sidecar.Id, error) {
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		return nil, sidecar.Id{}, err
	}
	signerId, err := sidecar.IdFromBytes(signed.GetSignerId())
	if err != nil {
		return nil, sidecar.Id{}, errors.New("a GenRotate naming no signer")
	}
	rotate := &protocol.GenRotate{}
	if err := proto.Unmarshal(signed.GetBody(), rotate); err != nil {
		return nil, signerId, err
	}
	return rotate, signerId, nil
}

// A Signed ReaderWrap, its KeyDelivery and signer, read unverified.
func readGkmConcurrencyDelivery(signedBytes []byte) (*bridge.Signed, *protocol.KeyDelivery, sidecar.Id, error) {
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		return nil, nil, sidecar.Id{}, err
	}
	signerId, err := sidecar.IdFromBytes(signed.GetSignerId())
	if err != nil {
		return signed, nil, sidecar.Id{}, errors.New("a delivery naming no signer")
	}
	wrapper := &protocol.ReaderWrap{}
	if err := proto.Unmarshal(signed.GetBody(), wrapper); err != nil {
		return signed, nil, signerId, err
	}
	if wrapper.GetWrap() == nil {
		return signed, nil, signerId, errors.New("a ReaderWrap with no KeyDelivery")
	}
	return signed, wrapper.GetWrap(), signerId, nil
}

// A claim's GenRotate verified under its signer's identity key, which identityKey resolves.
func gkmConcurrencyVerifyClaim(claim *gkmConcurrencyClaim, identityKey sidecar.GkmIdentityKeyFunction) error {
	if claim.err != nil {
		return claim.err
	}
	_, err := sidecar.ParseGenRotate(claim.signedBytes, claim.groupId, claim.rotate.GetGeneration(), identityKey)
	return err
}

// A delivery verified under its signer's identity key, which identityKey resolves, and held to its format.
func gkmConcurrencyVerifyDelivery(delivery *gkmConcurrencyDelivery, identityKey sidecar.GkmIdentityKeyFunction) error {
	if delivery.err != nil {
		return delivery.err
	}
	_, err := sidecar.ParseGkmDelivery(delivery.signed, sidecar.GkmWrapTypeReader, identityKey)
	return err
}

// A ReaderKey verified under its sidecar's own identity key, as a delivery to it may seal to it (§10.5's readerkey
// row), at the time it was published.
func gkmConcurrencyVerifyReaderKey(readerKey *gkmConcurrencyReaderKey, identityKey sidecar.GkmIdentityKeyFunction) error {
	if readerKey.err != nil {
		return readerKey.err
	}
	publicKey, err := identityKey(readerKey.sidecarId)
	if err != nil {
		return err
	}
	_, err = sidecar.CheckGkmReaderKey(readerKey.signed, readerKey.sidecarId, publicKey, readerKey.time.UnixMilli())
	return err
}

// Keeps one claim.
func (self *gkmConcurrencyOrigin) keepClaim(claim *gkmConcurrencyClaim) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.claims = append(self.claims, claim)
	}()
	self.changes.Update(func(count uint64) uint64 { return count + 1 })
}

// Every claim kept, oldest first.
func (self *gkmConcurrencyOrigin) claimList() []*gkmConcurrencyClaim {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.claims)
}

// Every delivery kept, oldest first.
func (self *gkmConcurrencyOrigin) deliveryList() []*gkmConcurrencyDelivery {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.deliveries)
}

// Every ReaderKey kept, oldest first.
func (self *gkmConcurrencyOrigin) readerKeyList() []*gkmConcurrencyReaderKey {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.readerKeys)
}

// Every late ask kept, oldest first.
func (self *gkmConcurrencyOrigin) askList() []*gkmConcurrencyAsk {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.asks)
}

// Every re-form's form and ladder call kept, oldest first.
func (self *gkmConcurrencyOrigin) formList() []*gkmConcurrencyForm {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.forms)
}

// The refusals recorded since the last call, oldest first.
func (self *gkmConcurrencyOrigin) takeRefusals() []*gkmConcurrencyRefusal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	refusals := slices.Clone(self.refusals[self.refusalsTaken:])
	self.refusalsTaken = len(self.refusals)
	return refusals
}

// One generation gkm took, as the claims show it: the GenRotate whose claim gkm answered with success, or one a
// re-form's ladder carried into a new group, naming the group it was claimed in.
type gkmConcurrencyGeneration struct {
	// the group gkm holds it in
	groupId  sidecar.Id
	repoId   sidecar.Id
	rotate   *protocol.GenRotate
	signerId sidecar.Id
	time     time.Time
	ladder   bool
}

// Each group's generations gkm took, by group and generation: the claims it answered with success, and a re-formed
// group's ladder, the old GenRotates byte for byte in the new group (§10.8's bytes row), from each ladder call it
// answered with success; and the forks: two generations gkm took at one (group, generation) under different
// commitments, which no linear ladder holds.
func gkmConcurrencyGenerations(claims []*gkmConcurrencyClaim, forms []*gkmConcurrencyForm) (map[sidecar.Id]map[uint64]*gkmConcurrencyGeneration, []string) {
	generations := map[sidecar.Id]map[uint64]*gkmConcurrencyGeneration{}
	forks := []string{}
	keep := func(groupId sidecar.Id, generation *gkmConcurrencyGeneration) {
		groupGenerations := generations[groupId]
		if groupGenerations == nil {
			groupGenerations = map[uint64]*gkmConcurrencyGeneration{}
			generations[groupId] = groupGenerations
		}
		g := generation.rotate.GetGeneration()
		if held, ok := groupGenerations[g]; ok {
			if !bytes.Equal(held.rotate.GetCommitment(), generation.rotate.GetCommitment()) {
				forks = append(forks, fmt.Sprintf("group %s's generation %d taken under commitments %x and %x", groupId, g, held.rotate.GetCommitment()[:8], generation.rotate.GetCommitment()[:8]))
			}
			return
		}
		groupGenerations[g] = generation
	}
	for _, claim := range claims {
		if !claim.taken() || claim.rotate == nil {
			continue
		}
		keep(claim.groupId, &gkmConcurrencyGeneration{groupId: claim.groupId, repoId: claim.repoId, rotate: claim.rotate, signerId: claim.signerId, time: claim.time})
	}
	for _, form := range forms {
		if !form.ladder || form.status != http.StatusOK && form.status != http.StatusCreated {
			continue
		}
		groupId, err := sidecar.IdFromBytes(form.form.GetGroupId())
		if err != nil {
			continue
		}
		for _, rotate := range form.generations {
			repoId, _ := sidecar.IdFromBytes(rotate.GetRepoId())
			keep(groupId, &gkmConcurrencyGeneration{groupId: groupId, repoId: repoId, rotate: rotate, time: form.time, ladder: true})
		}
	}
	return generations, forks
}

// Whether gkm took a re-form's form or ladder call.
func (self *gkmConcurrencyForm) taken() bool {
	return self.status == http.StatusOK || self.status == http.StatusCreated
}

// The taskworker stand-in: §10.6's reconciler on a host of its own (the file's header).
type gkmConcurrencyReconciler struct {
	reconciler *gkmservice.GkmReconciler
	budget     *evsservice.WriteBudgetClient
	closeGate  func()
	cancel     context.CancelFunc
}

// The reconciler on the world's clusters and Redis, until close.
func startGkmConcurrencyReconciler(world *World, settings *gkmservice.GkmReconcilerSettings) (*gkmConcurrencyReconciler, error) {
	ctx, cancel := context.WithCancel(context.Background())
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Blobs:         world.T3().Host().Blobs,
		ClockCheck:    sidecartest.NewFakeClockCheck(nil),
		ServiceTokens: world.T3().ServiceTokens(),
	})
	budget, err := evsservice.NewWriteBudget(ctx, host, nil, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("the stand-in's write budget: %w", err)
	}
	host.Budget = budget
	resolver, err := server.NewSidecarIdentityClientsResolver(&server.SidecarIdentityClientsResolverArgs{
		ApiUrl:     world.apiOrigin(),
		Bearer:     world.bearer(server.InternalGateCallerGkm),
		HttpClient: world.apiClient(),
	})
	if err != nil {
		budget.Close()
		cancel()
		return nil, err
	}
	writer, err := pushservice.NewHintWriter(host.Redis, nil, resolver, host.Metrics)
	if err != nil {
		budget.Close()
		cancel()
		return nil, err
	}
	host.Push = writer
	cache, closeGate, err := world.T3().AddGate("gkm-reconciler")
	if err != nil {
		budget.Close()
		cancel()
		return nil, err
	}
	reconciler, err := gkmservice.NewGkmReconciler(ctx, host, settings, &gkmservice.GkmReconcilerDeps{
		State: gkmservice.NewAclGkmReconcilerState(cache),
	})
	if err != nil {
		closeGate()
		budget.Close()
		cancel()
		return nil, err
	}
	return &gkmConcurrencyReconciler{reconciler: reconciler, budget: budget, closeGate: closeGate, cancel: cancel}, nil
}

// Ends the reconciler, its gate and its budget.
func (self *gkmConcurrencyReconciler) close() {
	self.reconciler.Close()
	self.closeGate()
	self.budget.Close()
	self.cancel()
}

// Files one of §7.9's or §8.8's statements as device, as sidecart3's FileStatement files it (its header at device's
// verified pin, a fresh nonce, a not_after the world's StatementLife from now, signed by device's identity key and
// posted to POST /network/config/delta with its token), then waits until the world's gates and every device of
// following holds the entry: the case's own sync, which waits on no revoked or closed device. The entry's pin, or api's
// refusal with its status and message.
func gkmConcurrencyFile(ctx context.Context, world *World, network *WorldNetwork, device *sidecart3.T3Device, signedType bridge.SignedType, body proto.Message, following []*sidecart3.T3Device, syncTimeout time.Duration) (*bridge.AuthPin, error) {
	follower := device.Follower()
	if follower == nil || follower.Client() == nil {
		return nil, fmt.Errorf("%s files by a follower that stands", device.Name)
	}
	purpose, ok := sidecar.ConfigStatementPurpose(signedType)
	if !ok {
		return nil, fmt.Errorf("%s is no statement of the log", signedType)
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	header := &bridge.StatementHeader{
		NetworkId:  sidecar.Id(network.NetworkId).Bytes(),
		At:         proto.Clone(follower.Client().Pin()).(*bridge.AuthPin),
		Nonce:      nonce,
		NotAfterMs: world.T3().Host().ClockOrReal().Now().Add(world.T3().Settings().StatementLife).UnixMilli(),
	}
	statement := proto.Clone(body)
	message := statement.ProtoReflect()
	field := message.Descriptor().Fields().ByName("header")
	if field == nil || field.Message() == nil || field.Message().FullName() != header.ProtoReflect().Descriptor().FullName() {
		return nil, fmt.Errorf("%s carries no StatementHeader", message.Descriptor().FullName())
	}
	message.Set(field, protoreflect.ValueOfMessage(header.ProtoReflect()))
	statementBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(statement)
	if err != nil {
		return nil, err
	}
	signed, err := device.Sidecar().Sign(purpose, signedType, statementBytes, nil)
	if err != nil {
		return nil, err
	}
	signedBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(signed)
	if err != nil {
		return nil, err
	}
	status, answer, err := world.T3().Api().Call(ctx, http.MethodPost, "/network/config/delta", device.Bearer(), &controller.NetworkConfigDeltaArgs{Signed: signedBytes})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s's %s: POST /network/config/delta: %d %s", device.Name, signedType, status, strings.TrimSpace(string(answer)))
	}
	result := &controller.NetworkConfigPinResult{}
	if err := json.Unmarshal(answer, result); err != nil || result.N == 0 {
		return nil, fmt.Errorf("%s's %s: POST /network/config/delta answered no pin", device.Name, signedType)
	}
	pin := &bridge.AuthPin{Version: result.N, Chain: result.Chain}
	return pin, gkmConcurrencySync(ctx, world, network, pin.GetVersion(), following, syncTimeout)
}

// Waits until every gate of the world and each of following holds network's log at version or past it.
func gkmConcurrencySync(ctx context.Context, world *World, network *WorldNetwork, version uint64, following []*sidecart3.T3Device, syncTimeout time.Duration) error {
	if err := world.T3().SyncGates(ctx, network.NetworkId, version); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	for _, device := range following {
		follower := device.Follower()
		if follower == nil || follower.Client() == nil {
			continue
		}
		if _, err := follower.Client().WaitFor(ctx, version); err != nil {
			return fmt.Errorf("%s did not follow %s's log to %d: %w", device.Name, network.Name, version, err)
		}
	}
	return nil
}

// A device's verified state's version, 0 with none.
func gkmConcurrencyStateVersion(device *sidecart3.T3Device) uint64 {
	follower := device.Follower()
	if follower == nil || follower.Client() == nil {
		return 0
	}
	return follower.Client().Pin().GetVersion()
}

// The network's config repo, as a device's verified state's genesis names it.
func gkmConcurrencyConfigRepoId(device *sidecart3.T3Device) (sidecar.Id, error) {
	follower := device.Follower()
	if follower == nil || follower.Client() == nil || follower.Client().State() == nil {
		return sidecar.Id{}, fmt.Errorf("%s follows no log", device.Name)
	}
	return sidecar.IdFromBytes(follower.Client().State().Genesis().GetConfigRepoId())
}

// What a device's key client re-forms with: the concrete key client's ReformGroup, which GkmClient does not carry.
type gkmConcurrencyReformer interface {
	ReformGroup(ctx context.Context, args *sidecar.GkmReformArgs) (sidecar.Id, error)
}
