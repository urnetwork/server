package main

// the fake load site: the end-to-end speed-test origin.
//
// GET / returns a JSON line listing child suburls followed by a padding body;
// the client crawls each suburl, which lists its own children, until the tree
// terminates. Each internal page has `branching` children, of which one
// continues deeper (a "next page" link) and the rest are leaves (sub-
// resources), so a crawl loads ~1 + depth*branching pages with a mean depth of
// K — bounded, but deep enough to exercise sequential discovery. Responses are
// deterministic from (seed, path) so runs reproduce, and carry a random-sized
// body so throughput is actually moved.
//
// The site listens on a real socket; providers egress to it over real OS
// sockets (loopback), which the disabled security policy permits.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/urnetwork/server"
)

const siteMaxDepth = 24

type siteHandler struct {
	seed     int64
	site     SiteConfig
	requests atomic.Uint64
}

type sitePage struct {
	Urls []string `json:"urls"`
	Size int      `json:"size"`
}

// Site is the running fake site.
type Site struct {
	server *http.Server
	addr   string
}

// NewSite starts the fake site on the given listen address (host:port, e.g.
// 127.0.0.1:0 for an ephemeral port). Returns the bound address.
func NewSite(ctx context.Context, listenAddr string, seed int64, site SiteConfig) (*Site, error) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}
	handler := &siteHandler{seed: seed, site: site}
	httpServer := &http.Server{Handler: handler}
	go server.HandleError(func() {
		httpServer.Serve(listener)
	})
	go func() {
		<-ctx.Done()
		httpServer.Close()
	}()
	return &Site{server: httpServer, addr: listener.Addr().String()}, nil
}

func (self *Site) Addr() string {
	return self.addr
}

func (self *siteHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// count requests that actually reach the origin (i.e. traversed the full
	// tunnel and provider egress), so a run can tell request-reaches-egress
	// from egress-return failures
	if n := self.requests.Add(1); n <= 5 || n%1000 == 0 {
		logf("fake site received request #%d: %s", n, r.URL.Path)
	}
	remaining, ok := self.parsePath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	pageRng := self.pathRng(r.URL.Path)

	if r.URL.Path == "/" {
		// draw the crawl depth (mean K), capped
		remaining = pageRng.geometricDepth(self.site.MeanDepth)
		if siteMaxDepth < remaining {
			remaining = siteMaxDepth
		}
	}

	bodySize := self.site.MinBodyBytes
	if self.site.MaxBodyBytes > self.site.MinBodyBytes {
		bodySize += pageRng.intn(self.site.MaxBodyBytes - self.site.MinBodyBytes)
	}

	urls := []string{}
	if 0 < remaining {
		branching := int(self.site.Branching)
		if branching < 1 {
			branching = 1
		}
		for i := 0; i < branching; i += 1 {
			childRemaining := 0
			if i == 0 {
				// one child continues deeper; the rest are leaves
				childRemaining = remaining - 1
			}
			token := self.childToken(r.URL.Path, i)
			urls = append(urls, fmt.Sprintf("/p/%d/%s", childRemaining, token))
		}
	}

	page := sitePage{Urls: urls, Size: bodySize}
	headerBytes, _ := json.Marshal(page)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(headerBytes)+1+bodySize))
	w.WriteHeader(http.StatusOK)
	w.Write(headerBytes)
	w.Write([]byte("\n"))
	writePadding(w, bodySize)
}

// parsePath returns the remaining depth encoded in the path. "/" is the root;
// "/p/<remaining>/<token>" is a crawl page.
func (self *siteHandler) parsePath(path string) (remaining int, ok bool) {
	if path == "/" {
		return 0, true
	}
	if !strings.HasPrefix(path, "/p/") {
		return 0, false
	}
	rest := strings.TrimPrefix(path, "/p/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return 0, false
	}
	remaining, err := strconv.Atoi(rest[:slash])
	if err != nil {
		return 0, false
	}
	return remaining, true
}

func (self *siteHandler) pathRng(path string) *rng {
	h := fnv.New64a()
	binary.Write(h, binary.BigEndian, self.seed)
	h.Write([]byte(path))
	return newRng(int64(h.Sum64()))
}

func (self *siteHandler) childToken(parentPath string, i int) string {
	h := fnv.New64a()
	binary.Write(h, binary.BigEndian, self.seed)
	h.Write([]byte(parentPath))
	binary.Write(h, binary.BigEndian, int64(i))
	return strconv.FormatUint(h.Sum64(), 16)
}

var paddingChunk = func() []byte {
	chunk := make([]byte, 32*1024)
	for i := range chunk {
		chunk[i] = byte('a' + i%26)
	}
	return chunk
}()

func writePadding(w http.ResponseWriter, n int) {
	for 0 < n {
		c := len(paddingChunk)
		if n < c {
			c = n
		}
		if _, err := w.Write(paddingChunk[:c]); err != nil {
			return
		}
		n -= c
	}
}
