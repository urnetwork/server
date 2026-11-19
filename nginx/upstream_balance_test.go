package nginx

// Tests of nginx's balancers. Each is checked against an independent port
// written here from nginx's C source (ngx_http_upstream_hash_module.c,
// ngx_http_upstream_least_conn_module.c, ngx_http_upstream_round_robin.c,
// ngx_crc32.c), and the consistent hash also against values printed by
// those C functions compiled from the pinned source, so a ring that is
// stable on its own but differs from nginx's fails.

import (
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"testing"
)

// The crc32 table as nginx's ngx_crc32_table256 holds it, generated from
// the reflected polynomial; two entries are checked against the C table.
func referenceCrc32Table(t *testing.T) [256]uint32 {
	var table [256]uint32
	for i := range 256 {
		c := uint32(i)
		for range 8 {
			if c&1 != 0 {
				c = 0xedb88320 ^ (c >> 1)
			} else {
				c >>= 1
			}
		}
		table[i] = c
	}
	// ngx_crc32_table256[1] and [255] in src/core/ngx_crc32.c
	if table[1] != 0x77073096 || table[255] != 0x2d02ef8d {
		t.Fatalf("reference crc32 table differs from nginx's")
	}
	return table
}

// ngx_crc32_update over a raw state.
func referenceCrc32Update(table *[256]uint32, crc uint32, p []byte) uint32 {
	for _, b := range p {
		crc = table[(crc^uint32(b))&0xff] ^ (crc >> 8)
	}
	return crc
}

// One point of the reference ring.
type referenceRingPoint struct {
	hash uint32
	peer int
}

// A peer of the reference upstream, in nginx's server order.
type referencePeer struct {
	name          string
	server        string
	weight        int
	currentWeight int
	conns         int
}

// ngx_http_upstream_update_chash: per server, crc32(host \0 port) as the
// base, then weight × 160 points, each the base updated with the previous
// point's 4 little-endian bytes and finalized; sorted stably by hash, as
// glibc's merge sort leaves equal points; duplicates dropped keeping the
// first.
func referenceRing(table *[256]uint32, peers []*referencePeer) []referenceRingPoint {
	points := []referenceRingPoint{}
	for peerIndex, peer := range peers {
		server := peer.server
		host, port := server, ""
		for j := 0; j < len(server); j++ {
			c := server[len(server)-j-1]
			if c == ':' {
				host = server[:len(server)-j-1]
				port = server[len(server)-j:]
				break
			}
			if c < '0' || c > '9' {
				break
			}
		}
		baseHash := uint32(0xffffffff)
		baseHash = referenceCrc32Update(table, baseHash, []byte(host))
		baseHash = referenceCrc32Update(table, baseHash, []byte{0})
		baseHash = referenceCrc32Update(table, baseHash, []byte(port))
		previousHash := uint32(0)
		for range peer.weight * 160 {
			hash := referenceCrc32Update(table, baseHash, []byte{
				byte(previousHash),
				byte(previousHash >> 8),
				byte(previousHash >> 16),
				byte(previousHash >> 24),
			})
			hash ^= 0xffffffff
			points = append(points, referenceRingPoint{hash: hash, peer: peerIndex})
			previousHash = hash
		}
	}
	slices.SortStableFunc(points, func(a referenceRingPoint, b referenceRingPoint) int {
		if a.hash < b.hash {
			return -1
		}
		if a.hash > b.hash {
			return 1
		}
		return 0
	})
	deduped := []referenceRingPoint{}
	for _, point := range points {
		if len(deduped) == 0 || deduped[len(deduped)-1].hash != point.hash {
			deduped = append(deduped, point)
		}
	}
	return deduped
}

// ngx_http_upstream_find_chash_point.
func referenceFindPoint(points []referenceRingPoint, hash uint32) uint32 {
	i, j := 0, len(points)
	for i < j {
		k := (i + j) / 2
		if hash > points[k].hash {
			i = k + 1
		} else if hash < points[k].hash {
			j = k
		} else {
			return uint32(k)
		}
	}
	return uint32(i)
}

// ngx_http_upstream_get_peer: smooth weighted round robin over untried
// peers. -1 for none.
func referenceRoundRobin(peers []*referencePeer, tried []bool) int {
	best := -1
	total := 0
	for i, peer := range peers {
		if tried[i] {
			continue
		}
		peer.currentWeight += peer.weight
		total += peer.weight
		if best < 0 || peer.currentWeight > peers[best].currentWeight {
			best = i
		}
	}
	if best < 0 {
		return -1
	}
	peers[best].currentWeight -= total
	return best
}

// One request's walk on the reference ring: init_chash_peer, then
// get_chash_peer for each attempt. fellBack reports a round robin pick.
type referenceWalk struct {
	hash  uint32
	tries int
}

func referenceChashPeer(points []referenceRingPoint, peers []*referencePeer, walk *referenceWalk, tried []bool) (peer int, fellBack bool) {
	if walk.tries > 20 || len(peers) == 1 {
		return referenceRoundRobin(peers, tried), true
	}
	for {
		peer := points[walk.hash%uint32(len(points))].peer
		if !tried[peer] {
			return peer, false
		}
		walk.hash++
		walk.tries++
		if walk.tries > 20 {
			return referenceRoundRobin(peers, tried), true
		}
	}
}

// A group over the given blocks with no sockets: selection only.
func newSelectionGroup(t *testing.T, serviceName string, stateful bool, testBlocks []testBlock) *upstreamGroup {
	service := testRouteService(serviceName, testBlocks)
	service.Stateful = stateful
	group, _ := newTestGroup(t, service, testFrontSettings())
	return group
}

// The production pick of a ring walk, by block name.
func ringPick(group *upstreamGroup, walk *upstreamRingWalk, triedBlocks []bool) string {
	group.stateLock.Lock()
	defer group.stateLock.Unlock()
	block := group.selectRingWithLock(walk, triedBlocks)
	if block == nil {
		return ""
	}
	triedBlocks[block.index] = true
	return block.name
}

// The ring and its picks, against values printed by nginx's own C functions
// (ngx_crc32_*, ngx_http_upstream_update_chash, find_chash_point and the
// get_chash_peer walk) compiled from the pinned source: ring sizes and edge
// points, and each fixed client's first block and the block its walk
// reaches when the first is tried, under main's weights and a weight change.
func TestUpstreamRingMatchesNginxGoldenValues(t *testing.T) {
	type goldenPick struct {
		key    string
		first  string
		second string
	}
	type goldenConfig struct {
		blocks     []testBlock
		pointCount int
		edgePoints []upstreamRingPoint
		lastPoint  upstreamRingPoint
		picks      []goldenPick
	}
	blockA := func(name string) *upstreamBlock { return &upstreamBlock{name: name} }
	configs := []goldenConfig{
		{
			blocks: []testBlock{
				{name: "beta", weight: 1, port: 7101},
				{name: "g1", weight: 24, port: 7102},
				{name: "g2", weight: 25, port: 7103},
				{name: "g3", weight: 25, port: 7104},
				{name: "g4", weight: 25, port: 7105},
			},
			pointCount: 16000,
			edgePoints: []upstreamRingPoint{
				{hash: 236548, block: blockA("g4")},
				{hash: 554795, block: blockA("g2")},
				{hash: 624332, block: blockA("g1")},
			},
			lastPoint: upstreamRingPoint{hash: 4294207341, block: blockA("g2")},
			picks: []goldenPick{
				{key: "192.0.2.1", first: "g4", second: "g1"},
				{key: "192.0.2.77", first: "g2", second: "g3"},
				{key: "198.51.100.23", first: "g3", second: "g2"},
				{key: "203.0.113.250", first: "g2", second: "g3"},
				{key: "2001:db8::1", first: "g1", second: "g2"},
				{key: "2001:db8:1:2::abcd", first: "g1", second: "g3"},
				{key: "2001:db8:ffff::42", first: "g2", second: "g1"},
				{key: "::192.0.2.9", first: "g4", second: "g3"},
			},
		},
		{
			blocks: []testBlock{
				{name: "beta", weight: 1, port: 7101},
				{name: "g1", weight: 49, port: 7102},
				{name: "g2", weight: 1, port: 7103},
				{name: "g3", weight: 25, port: 7104},
				{name: "g4", weight: 25, port: 7105},
			},
			pointCount: 16160,
			edgePoints: []upstreamRingPoint{
				{hash: 236548, block: blockA("g4")},
				{hash: 609694, block: blockA("g1")},
				{hash: 624332, block: blockA("g1")},
			},
			lastPoint: upstreamRingPoint{hash: 4293981070, block: blockA("g1")},
			picks: []goldenPick{
				{key: "192.0.2.1", first: "g4", second: "g1"},
				{key: "192.0.2.77", first: "g1", second: "g3"},
				{key: "198.51.100.23", first: "g3", second: "g4"},
				{key: "203.0.113.250", first: "g3", second: "g2"},
				{key: "2001:db8::1", first: "g1", second: "g3"},
				{key: "2001:db8:1:2::abcd", first: "g1", second: "g3"},
				{key: "2001:db8:ffff::42", first: "g1", second: "g4"},
				{key: "::192.0.2.9", first: "g4", second: "g3"},
			},
		},
		{
			blocks: []testBlock{
				{name: "g1", weight: 2, port: 7201},
				{name: "g2", weight: 3, port: 7202},
			},
			pointCount: 800,
			edgePoints: []upstreamRingPoint{
				{hash: 4503891, block: blockA("g1")},
				{hash: 4951641, block: blockA("g1")},
				{hash: 5592472, block: blockA("g2")},
			},
			lastPoint: upstreamRingPoint{hash: 4284029734, block: blockA("g1")},
			picks: []goldenPick{
				{key: "192.0.2.1", first: "g1", second: "g2"},
				{key: "192.0.2.77", first: "g2", second: "g1"},
				{key: "198.51.100.23", first: "g1", second: "g2"},
				{key: "203.0.113.250", first: "g2", second: "g1"},
				{key: "2001:db8::1", first: "g2", second: "g1"},
				{key: "2001:db8:1:2::abcd", first: "g1", second: "g2"},
				{key: "2001:db8:ffff::42", first: "g2", second: "g1"},
				{key: "::192.0.2.9", first: "g1", second: "g2"},
			},
		},
	}
	for i, config := range configs {
		group := newSelectionGroup(t, fmt.Sprintf("golden-%d", i), true, config.blocks)
		if len(group.ring) != config.pointCount {
			t.Fatalf("config %d: %d ring points, want %d", i, len(group.ring), config.pointCount)
		}
		for j, edgePoint := range config.edgePoints {
			point := group.ring[j]
			if point.hash != edgePoint.hash || point.block.name != edgePoint.block.name {
				t.Errorf("config %d point %d: %d %s, want %d %s", i, j, point.hash, point.block.name, edgePoint.hash, edgePoint.block.name)
			}
		}
		lastPoint := group.ring[len(group.ring)-1]
		if lastPoint.hash != config.lastPoint.hash || lastPoint.block.name != config.lastPoint.block.name {
			t.Errorf("config %d last point: %d %s, want %d %s", i, lastPoint.hash, lastPoint.block.name, config.lastPoint.hash, config.lastPoint.block.name)
		}
		for _, pick := range config.picks {
			keyText := nginxRemoteAddrText(netip.MustParseAddr(pick.key))
			walk := newUpstreamRingWalk(keyText)
			triedBlocks := make([]bool, len(group.blocks))
			first := ringPick(group, walk, triedBlocks)
			second := ringPick(group, walk, triedBlocks)
			if first != pick.first || second != pick.second {
				t.Errorf("config %d client %s: %s then %s, want %s then %s", i, pick.key, first, second, pick.first, pick.second)
			}
		}
	}
}

// The ring against the independent port, over fixed ipv4 and ipv6 clients,
// several weight sets, and walks past one, two and three tried blocks,
// with the round robin fallback reached at least once.
func TestUpstreamRingMatchesReference(t *testing.T) {
	table := referenceCrc32Table(t)
	weightSets := [][]int{
		{1, 24, 25, 25, 25},
		{1, 49, 1, 25, 25},
		{100, 1, 1},
		{3, 3, 3, 3},
		{7},
	}
	keyTexts := []string{}
	for i := range 64 {
		keyTexts = append(keyTexts, "192.0.2."+strconv.Itoa(i*4))
		keyTexts = append(keyTexts, "198.51.100."+strconv.Itoa(255-i))
		keyTexts = append(keyTexts, "2001:db8::"+strconv.FormatInt(int64(i*97+1), 16))
		keyTexts = append(keyTexts, "2001:db8:"+strconv.FormatInt(int64(i+1), 16)+"::"+strconv.FormatInt(int64(i*13+7), 16))
	}
	fellBackCount := 0
	for setIndex, weights := range weightSets {
		testBlocks := []testBlock{}
		peers := []*referencePeer{}
		for i, weight := range weights {
			name := fmt.Sprintf("b%d", i)
			port := 7300 + i
			testBlocks = append(testBlocks, testBlock{name: name, weight: weight, port: port})
			peers = append(peers, &referencePeer{name: name, server: "warpservices:" + strconv.Itoa(port), weight: weight})
		}
		group := newSelectionGroup(t, fmt.Sprintf("reference-%d", setIndex), true, testBlocks)
		points := referenceRing(&table, peers)
		if len(points) != len(group.ring) {
			t.Fatalf("set %d: %d points, reference %d", setIndex, len(group.ring), len(points))
		}
		for i := range points {
			if points[i].hash != group.ring[i].hash || peers[points[i].peer].name != group.ring[i].block.name {
				t.Fatalf("set %d point %d differs from the reference", setIndex, i)
			}
		}
		for _, keyText := range keyTexts {
			// the production key is nginx's text of the parsed address
			addr := netip.MustParseAddr(keyText)
			if nginxRemoteAddrText(addr) != keyText {
				t.Fatalf("key %s prints as %s", keyText, nginxRemoteAddrText(addr))
			}
			walk := newUpstreamRingWalk(nginxRemoteAddrText(addr))
			referenceWalk := &referenceWalk{
				hash: referenceFindPoint(points, referenceCrc32Update(&table, 0xffffffff, []byte(keyText))^0xffffffff),
			}
			triedBlocks := make([]bool, len(group.blocks))
			referenceTried := make([]bool, len(peers))
			for attempt := range min(len(peers), 4) {
				got := ringPick(group, walk, triedBlocks)
				wantPeer, fellBack := referenceChashPeer(points, peers, referenceWalk, referenceTried)
				if fellBack {
					fellBackCount += 1
				}
				want := ""
				if 0 <= wantPeer {
					referenceTried[wantPeer] = true
					want = peers[wantPeer].name
				}
				if got != want {
					t.Fatalf("set %d client %s attempt %d: %s, reference %s", setIndex, keyText, attempt, got, want)
				}
			}
		}
	}
	if fellBackCount == 0 {
		t.Errorf("no walk reached the round robin fallback")
	}
}

// least_conn as ngx_http_upstream_get_least_conn_peer computes it: the
// fewest conns per weight, ties by smooth weighted round robin from the
// first tied peer.
func referenceLeastConn(peers []*referencePeer) int {
	best := -1
	many := false
	for i, peer := range peers {
		if best < 0 || peer.conns*peers[best].weight < peers[best].conns*peer.weight {
			best = i
			many = false
		} else if peer.conns*peers[best].weight == peers[best].conns*peer.weight {
			many = true
		}
	}
	if many {
		total := 0
		for i := best; i < len(peers); i++ {
			peer := peers[i]
			if peer.conns*peers[best].weight != peers[best].conns*peer.weight {
				continue
			}
			peer.currentWeight += peer.weight
			total += peer.weight
			if peer.currentWeight > peers[best].currentWeight {
				best = i
			}
		}
		peers[best].currentWeight -= total
	}
	peers[best].conns += 1
	return best
}

// Weighted least_conn under requests held open at the blocks: each new
// request goes where the reference sends it, as requests are held and
// released.
func TestUpstreamLeastConnUnderHeldRequests(t *testing.T) {
	weights := []int{1, 2, 3}
	upstreams := []*recordingUpstream{}
	releases := []chan struct{}{}
	testBlocks := []testBlock{}
	peers := []*referencePeer{}
	arrivals := make(chan *recordedRequest)
	for i, weight := range weights {
		name := fmt.Sprintf("g%d", i+1)
		release := make(chan struct{}, 64)
		upstream := newRecordingUpstream(t, name, func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
			<-release
			io.WriteString(w, recorded.block)
		})
		upstream.arrivals = arrivals
		upstreams = append(upstreams, upstream)
		releases = append(releases, release)
		testBlocks = append(testBlocks, testBlock{name: name, weight: weight, port: upstream.port})
		peers = append(peers, &referencePeer{name: name, weight: weight})
	}
	group, _ := newTestGroup(t, testRouteService("least-conn", testBlocks), testFrontSettings())
	front := newTestFront(t, group, testForwardOptions)

	send := func() string {
		go func() {
			request, _ := http.NewRequest(http.MethodGet, front.url("/held"), nil)
			testDo(t, request)
		}()
		return (<-arrivals).block
	}
	release := func(blockIndex int) {
		releases[blockIndex] <- struct{}{}
		front.waitServed(t)
		peers[blockIndex].conns -= 1
	}

	check := func(step string) {
		got := send()
		want := peers[referenceLeastConn(peers)].name
		if got != want {
			t.Fatalf("%s: request went to %s, reference %s", step, got, want)
		}
	}
	for i := range 12 {
		check(fmt.Sprintf("held %d", i))
	}
	release(2)
	release(2)
	release(0)
	for i := range 6 {
		check(fmt.Sprintf("after release %d", i))
	}
	for i, release := range releases {
		for range len(upstreams[i].recorded()) {
			release <- struct{}{}
		}
	}
}

// A block of weight 0 gets no balanced request, but a request naming it
// reaches it.
func TestUpstreamWeightZeroBlock(t *testing.T) {
	zero := newRecordingUpstream(t, "g0", nil)
	live := newRecordingUpstream(t, "g1", nil)
	group, _ := newTestGroup(t, testRouteService("weight-zero", []testBlock{
		{name: "g0", weight: 0, port: zero.port},
		{name: "g1", weight: 1, port: live.port},
	}), testFrontSettings())
	front := newTestFront(t, group, func(r *http.Request) forwardOptions {
		options := testForwardOptions(r)
		options.blockName = r.URL.Query().Get("block")
		return options
	})
	for range 8 {
		request, _ := http.NewRequest(http.MethodGet, front.url("/"), nil)
		if _, body, err := testDo(t, request); err != nil || string(body) != "g1" {
			t.Fatalf("balanced request reached %q %v, want g1", body, err)
		}
	}
	request, _ := http.NewRequest(http.MethodGet, front.url("/status?block=g0"), nil)
	if _, body, err := testDo(t, request); err != nil || string(body) != "g0" {
		t.Fatalf("named request reached %q %v, want g0", body, err)
	}
	if len(zero.recorded()) != 1 {
		t.Errorf("weight-zero block saw %d requests, want only the named one", len(zero.recorded()))
	}
}

// A block at ForwardSettings.MaxBlockActiveRequests is skipped, and with
// every block at its cap the request is nginx's "no live upstreams", a 502.
func TestUpstreamBlockCapAndNoLiveUpstreams(t *testing.T) {
	settings := testFrontSettings()
	settings.Forward.MaxBlockActiveRequests = 1
	release := make(chan struct{})
	arrivals := make(chan *recordedRequest)
	handler := func(w http.ResponseWriter, r *http.Request, recorded *recordedRequest) {
		<-release
	}
	first := newRecordingUpstream(t, "g1", handler)
	second := newRecordingUpstream(t, "g2", handler)
	first.arrivals = arrivals
	second.arrivals = arrivals
	serviceName := testServiceName("block-cap")
	group, _ := newTestGroup(t, testRouteService(serviceName, []testBlock{
		{name: "g1", weight: 1, port: first.port},
		{name: "g2", weight: 1, port: second.port},
	}), settings)
	front := newTestFront(t, group, testForwardOptions)
	for range 2 {
		go func() {
			request, _ := http.NewRequest(http.MethodGet, front.url("/held"), nil)
			testDo(t, request)
		}()
	}
	heldBlocks := []string{(<-arrivals).block, (<-arrivals).block}
	slices.Sort(heldBlocks)
	if !slices.Equal(heldBlocks, []string{"g1", "g2"}) {
		t.Fatalf("held requests went to %v, want one per block", heldBlocks)
	}
	request, _ := http.NewRequest(http.MethodGet, front.url("/third"), nil)
	response, _, err := testDo(t, request)
	if err != nil || response.StatusCode != http.StatusBadGateway {
		t.Fatalf("with every block at its cap: %v %v, want 502", response, err)
	}
	if forwardErrorCount(serviceName, http.StatusBadGateway) != 1 {
		t.Errorf("the 502 was not counted")
	}
	close(release)
}
