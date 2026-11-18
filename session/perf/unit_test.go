package perf

import (
	"math/rand/v2"
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Isolated costs: each operation alone, for a fixed count, with the cluster's
// CPU and commands between snapshots. Networks stay far below every cap.
func (self *harness) runUnitPhase() {
	samples := self.config.profile.unitSamples
	networkCount := max(2, samples/100)
	networks := make([]*perfNetwork, networkCount)
	runParallel(networkCount, 8, func(index int) {
		networks[index] = self.newNetwork("unit")
	})

	// password sign-ins: one new independent session each
	signIns := make([]*credential, samples)
	self.measure("mint.signin", samples, func() {
		runParallel(samples, 16, func(index int) {
			start := time.Now()
			credential, err := self.signIn(networks[index%networkCount])
			self.ops.observe("unit.mint.signin", start, err)
			signIns[index] = credential
		})
	})

	// auth-client with an API key: a new client and its own session
	type created struct {
		network    *perfNetwork
		credential *credential
		clientId   server.Id
	}
	apiKeyClients := make([]*created, samples)
	self.measure("authclient.apikey", samples, func() {
		runParallel(samples, 16, func(index int) {
			network := networks[index%networkCount]
			clientSession := self.apiKeySession(network)
			defer clientSession.Cancel()
			start := time.Now()
			credential, clientId, _, err := self.authClient(clientSession, false)
			self.ops.observe("unit.authclient.apikey", start, err)
			if err == nil {
				apiKeyClients[index] = &created{network: network, credential: credential, clientId: clientId}
			}
		})
	})

	// auth-client with a signed-in network credential: the client inherits
	jwtClients := make([]*created, samples)
	self.measure("authclient.jwt", samples, func() {
		runParallel(samples, 16, func(index int) {
			signIn := signIns[index]
			if signIn == nil {
				return
			}
			network := networks[index%networkCount]
			clientSession := self.credentialSession(signIn)
			defer clientSession.Cancel()
			start := time.Now()
			credential, clientId, _, err := self.authClient(clientSession, false)
			self.ops.observe("unit.authclient.jwt", start, err)
			if err == nil {
				jwtClients[index] = &created{network: network, credential: credential, clientId: clientId}
			}
		})
	})

	// a hosted device reopening a client that already has its session
	self.measure("mint.hosted.reuse", samples, func() {
		runParallel(samples, 16, func(index int) {
			client := apiKeyClients[index]
			if client == nil {
				return
			}
			start := time.Now()
			_, err := self.hostedMint(client.network, *client.credential.claims.DeviceId, client.clientId)
			self.ops.observe("unit.mint.hosted.reuse", start, err)
		})
	})

	// hosted derived children: a child row inheriting the parent's session
	children := make([]*created, samples)
	self.measure("mint.child", samples, func() {
		runParallel(samples, 16, func(index int) {
			parent := apiKeyClients[index]
			if parent == nil {
				return
			}
			start := time.Now()
			credential, clientId, err := self.childMint(parent.credential)
			self.ops.observe("unit.mint.child", start, err)
			if err == nil {
				children[index] = &created{network: parent.network, credential: credential, clientId: clientId}
			}
		})
	})

	// refresh of a tagged network credential
	self.measure("refresh", samples, func() {
		runParallel(samples, 16, func(index int) {
			signIn := signIns[index]
			if signIn == nil {
				return
			}
			start := time.Now()
			_, err := self.refresh(signIn, networks[index%networkCount])
			self.ops.observe("unit.refresh", start, err)
		})
	})

	// first checks: the record has no projection yet, so SQL answers and
	// projects
	checkable := []*credential{}
	for _, client := range jwtClients {
		if client != nil {
			checkable = append(checkable, client.credential)
		}
	}
	for _, client := range apiKeyClients {
		if client != nil {
			checkable = append(checkable, client.credential)
		}
	}
	self.measure("check.miss", len(checkable), func() {
		runParallel(len(checkable), 16, func(index int) {
			start := time.Now()
			err := self.leaseCheck(checkable[index].claims)
			self.ops.observe("unit.check.miss", start, err)
		})
	})

	// warm checks: one HMGET each
	runs := self.config.profile.unitCheckRuns
	self.measure("check.hit", runs, func() {
		runParallel(runs, 32, func(index int) {
			credential := checkable[index%len(checkable)]
			start := time.Now()
			err := self.leaseCheck(credential.claims)
			self.ops.observe("unit.check.hit", start, err)
		})
	})

	// the same, sequential: the REVOKE-OPERATIONS reference was a sequential
	// pass of 1,000 checks
	self.measure("check.hit.sequential", 1000, func() {
		for index := range 1000 {
			credential := checkable[index%len(checkable)]
			start := time.Now()
			err := self.leaseCheck(credential.claims)
			self.ops.observe("unit.check.hit.sequential", start, err)
		}
	})

	// API request authentication of client credentials
	self.measure("api.auth.client", runs, func() {
		runParallel(runs, 32, func(index int) {
			credential := checkable[index%len(checkable)]
			start := time.Now()
			err := self.apiAuth(credential.jwt)
			self.ops.observe("unit.api.auth.client", start, err)
		})
	})

	// API request authentication of network credentials (the record is keyed
	// by the session)
	networkCredentials := []*credential{}
	for _, signIn := range signIns {
		if signIn != nil {
			networkCredentials = append(networkCredentials, signIn)
		}
	}
	self.measure("api.auth.network", runs, func() {
		runParallel(runs, 32, func(index int) {
			credential := networkCredentials[index%len(networkCredentials)]
			start := time.Now()
			err := self.apiAuth(credential.jwt)
			self.ops.observe("unit.api.auth.network", start, err)
		})
	})

	// hint reads and lists of small networks
	self.measure("hint.small", samples, func() {
		runParallel(samples, 16, func(index int) {
			start := time.Now()
			_, _, err := session.NetworkSessionRevision(self.ctx, networks[index%networkCount].id)
			self.ops.observe("unit.hint.small", start, err)
		})
	})
	listCount := max(10, samples/10)
	self.measure("list.small", listCount, func() {
		runParallel(listCount, 4, func(index int) {
			actor := self.credentialSession(networkCredentials[index%len(networkCredentials)])
			defer actor.Cancel()
			start := time.Now()
			_, err := session.GetNetworkSessions(actor)
			self.ops.observe("unit.list.small", start, err)
		})
	})

	// auth codes: create, then redeem as an independent session
	codeCount := max(20, samples/5)
	codes := make([]string, codeCount)
	self.measure("code.create", codeCount, func() {
		runParallel(codeCount, 8, func(index int) {
			start := time.Now()
			code, err := self.codeCreate(networkCredentials[index%len(networkCredentials)])
			self.ops.observe("unit.code.create", start, err)
			codes[index] = code
		})
	})
	self.measure("code.login", codeCount, func() {
		runParallel(codeCount, 8, func(index int) {
			if codes[index] == "" {
				return
			}
			start := time.Now()
			_, err := self.codeLogin(codes[index])
			self.ops.observe("unit.code.login", start, err)
		})
	})

	// single revocations by another sign-in of the same network, at most 100
	// per network a day
	revokeCount := min(networkCount*50, max(10, samples/5))
	byNetwork := map[server.Id][]*credential{}
	for index, signIn := range signIns {
		if signIn != nil {
			network := networks[index%networkCount]
			byNetwork[network.id] = append(byNetwork[network.id], signIn)
		}
	}
	type revocation struct {
		actor  *credential
		target server.Id
	}
	revocations := []revocation{}
	for _, list := range byNetwork {
		// the actor list[0] stays; the single-revoke quota is 100 a network
		for index := 1; index < len(list) && index <= 50 && len(revocations) < revokeCount; index++ {
			revocations = append(revocations, revocation{actor: list[0], target: *list[index].claims.SessionId})
		}
	}
	self.measure("revoke.single", len(revocations), func() {
		runParallel(len(revocations), 4, func(index int) {
			item := revocations[index]
			actor := self.credentialSession(item.actor)
			defer actor.Cancel()
			start := time.Now()
			_, err := session.RevokeNetworkSession(&session.RevokeSessionArgs{SessionId: item.target, OperationId: server.NewId()}, actor)
			self.ops.observe("unit.revoke.single", start, err)
		})
	})

	// removals: an API key client owns its session; a network credential's
	// client shares it
	removeOne := func(name string, clients []*created) {
		live := []*created{}
		for _, client := range clients {
			if client != nil {
				live = append(live, client)
			}
		}
		self.measure(name, len(live), func() {
			runParallel(len(live), 16, func(index int) {
				client := live[index]
				remover := self.apiKeySession(client.network)
				defer remover.Cancel()
				start := time.Now()
				err := self.removeClient(remover, client.clientId)
				self.ops.observe("unit."+name, start, err)
			})
		})
	}
	removeOne("remove.client.jwt", jwtClients)
	// each API key client owns its session: R-free expects its removal to
	// free it from the live set
	liveBefore := self.totalLive(networks)
	removeOne("remove.client.apikey", apiKeyClients)
	liveAfter := self.totalLive(networks)
	owned := 0
	for _, client := range apiKeyClients {
		if client != nil {
			owned++
		}
	}
	self.report.addOutcome("sessions freed by removing API key clients (R-free expects all)", "freed", int(liveBefore-liveAfter), owned-int(liveBefore-liveAfter))

	// the record answers for removed clients from their tombstones
	var refused, accepted int
	var lock sync.Mutex
	runParallel(min(len(checkable), 500), 16, func(index int) {
		credential := checkable[rand.IntN(len(checkable))]
		err := self.leaseCheck(credential.claims)
		lock.Lock()
		defer lock.Unlock()
		if err == nil {
			accepted++
		} else {
			refused++
		}
	})
	self.report.addOutcome("removed clients' credentials at their next check", "refused", refused, accepted)
}

func (self *harness) totalLive(networks []*perfNetwork) int64 {
	var total int64
	for _, network := range networks {
		total += self.liveSessionCount(network.id)
	}
	return total
}
