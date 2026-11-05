// Private receipts retain local route outcomes before healthy findings are filtered.
package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// The association is immutable. Completion belongs to this invocation alone;
// another invocation cannot replace a shared "latest receipt" pointer.
type observerRouteInvocationKey struct{}
type observerRouteInvocation struct {
	runId, policySha256 string
	invocation          uint64
	identity            settingsWitnessIdentity
	completion          *observerRouteCompletion
}

type observerRouteCompletion struct {
	stateLock sync.Mutex
	claimed   bool
	sha       string
	status    string
}

func (self *observerRouteCompletion) claim() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.claimed {
		return false
	}
	self.claimed = true
	return true
}

func (self *observerRouteCompletion) finish(sha, status string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sha, self.status = sha, status
}

func (self *observerRouteCompletion) result() (string, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sha, self.status
}

// Only the local route policy and desired scope enter this private digest.
// Credentials, route answers and other service configuration are never hashed.
type observerRoutePolicyTarget struct {
	Name, Address string
	Disabled      bool
}
type observerRoutePolicy struct {
	Environment string
	Mode        string
	Contract    ObserverOverlaySettings
	Settings    ObserverRouteSettings
	Targets     []observerRoutePolicyTarget
	Excluded    []string
}

func observerRouteExcluded(cfg *monitorConfig, excluded []string) []string {
	routeNames := map[string]bool{}
	for _, hosts := range [][]*host{cfg.hosts, cfg.disabledHosts} {
		for _, h := range hosts {
			routeNames[h.name] = true
		}
	}
	names := map[string]bool{}
	for _, name := range excluded {
		if routeNames[name] {
			names[name] = true
		}
	}
	// Match the existing scope guard, but unrelated disabled routers are not
	// members of this probe's overlay inventory or route-policy digest.
	for _, h := range cfg.scopeHosts() {
		if h.disabled && routeNames[h.name] {
			names[h.name] = true
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func observerRouteEffectivePolicy(cfg *monitorConfig, excluded []string, settings *ObserverRouteSettings) observerRoutePolicy {
	p := observerRoutePolicy{Environment: cfg.env, Mode: cfg.addressMode, Contract: cloneObserverOverlaySettings(cfg.observerOverlay), Settings: *settings, Excluded: observerRouteExcluded(cfg, excluded)}
	for _, hosts := range [][]*host{cfg.hosts, cfg.disabledHosts} {
		for _, h := range hosts {
			p.Targets = append(p.Targets, observerRoutePolicyTarget{Name: h.name, Address: h.overlayIp, Disabled: h.disabled})
		}
	}
	return p
}

// Reject oversized input before encoding; a bounded prefix is never a digest.
func observerRoutePolicyDigest(p observerRoutePolicy, maxBytes int) string {
	size := len(p.Environment) + len(p.Mode)
	for _, values := range [][]string{p.Contract.Interfaces, p.Contract.Gateways, p.Excluded} {
		for _, v := range values {
			size += len(v) + 8
			if size > maxBytes {
				return ""
			}
		}
	}
	for _, t := range p.Targets {
		size += len(t.Name) + len(t.Address) + 64
		if size > maxBytes {
			return ""
		}
	}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > maxBytes {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Per-target references are private inventory-slot digests, never raw names,
// addresses, interfaces, gateways or route output. The policy digest pins order.
type observerRouteTargetReceipt struct {
	Slot         int    `json:"slot"`
	TargetSha256 string `json:"target_sha256"`
	Outcome      string `json:"outcome"`
	Complete     bool   `json:"complete"`
}

type observerRouteReceipt struct {
	Schema                    int                          `json:"schema"`
	CycleId                   string                       `json:"cycle_id"`
	StartedAt                 time.Time                    `json:"started_at"`
	FinishedAt                time.Time                    `json:"finished_at"`
	WatcherRunId              string                       `json:"watcher_run_id,omitempty"`
	Invocation                uint64                       `json:"invocation,omitempty"`
	SourceIdentity            *settingsWitnessIdentity     `json:"source_identity,omitempty"`
	SourceAssociationComplete bool                         `json:"source_association_complete"`
	PolicySha256              string                       `json:"policy_sha256"`
	PolicyUnchanged           bool                         `json:"policy_unchanged"`
	PolicyAssociationComplete bool                         `json:"policy_association_complete"`
	Mode                      string                       `json:"mode"`
	Reason                    string                       `json:"reason"`
	Complete                  bool                         `json:"complete"`
	ContextEnded              bool                         `json:"context_ended"`
	Targets                   []observerRouteTargetReceipt `json:"targets"`
	TargetsOmitted            int                          `json:"targets_omitted"`
}

type observerRouteReceiptOwner struct {
	receipt     observerRouteReceipt
	env         *probeEnv
	settings    *ObserverRouteSettings
	excluded    []string
	association *observerRouteInvocation
	claimed     bool
}

func beginObserverRouteReceipt(ctx context.Context, env *probeEnv, settings *ObserverRouteSettings) *observerRouteReceiptOwner {
	association, _ := ctx.Value(observerRouteInvocationKey{}).(*observerRouteInvocation)
	claimed := association != nil && association.completion != nil && association.completion.claim()
	if env.cfg.stateDir == "" {
		if claimed {
			association.completion.finish("", "disabled")
		}
		return nil
	}
	self := &observerRouteReceiptOwner{env: env, settings: settings, association: association, claimed: claimed}
	if scoped, ok := env.runner.(*hostScopeRunner); ok {
		self.excluded = scoped.excludedNames()
	}
	p := observerRouteEffectivePolicy(env.cfg, self.excluded, settings)
	r := observerRouteReceipt{Schema: 1, StartedAt: env.now().UTC(), Reason: "not-observed", Mode: string(p.Mode), Targets: []observerRouteTargetReceipt{}}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err == nil {
		r.CycleId = hex.EncodeToString(nonce[:])
	}
	r.PolicySha256 = observerRoutePolicyDigest(p, settings.MaxReceiptBytes)
	if claimed && settingsWitnessHex(association.runId, 32) && association.invocation > 0 && association.identity.valid() {
		r.WatcherRunId, r.Invocation = association.runId, association.invocation
		identity := association.identity
		r.SourceIdentity = &identity
		r.SourceAssociationComplete = true
		r.PolicyAssociationComplete = r.PolicySha256 != "" && r.PolicySha256 == association.policySha256
	}
	excluded := map[string]bool{}
	for _, name := range p.Excluded {
		excluded[name] = true
	}
	for i, target := range p.Targets {
		if i >= settings.MaxReceiptTargets {
			r.TargetsOmitted++
			continue
		}
		targetSha256 := ""
		if len(target.Name)+len(target.Address) <= settings.MaxReceiptBytes {
			raw, _ := json.Marshal(target)
			sum := sha256.Sum256(raw)
			targetSha256 = hex.EncodeToString(sum[:])
		}
		state := "not-observed"
		if target.Disabled {
			state = "disabled"
		} else if excluded[target.Name] {
			state = "excluded"
		}
		r.Targets = append(r.Targets, observerRouteTargetReceipt{Slot: i, TargetSha256: targetSha256, Outcome: state})
	}
	self.receipt = r
	return self
}

func (self *observerRouteReceiptOwner) reason(reason string) {
	if self != nil {
		self.receipt.Reason = reason
	}
}

func (self *observerRouteReceiptOwner) target(slot int, outcome string) {
	if self != nil && slot < len(self.receipt.Targets) {
		self.receipt.Targets[slot].Outcome = outcome
		self.receipt.Targets[slot].Complete = outcome == "conforming" || outcome == "diverted" || outcome == "absent"
	}
}

// Publication is deferred even for a canceled owner. It records that lifecycle
// as incomplete; it never turns cancellation or a zero-target run into health.
func (self *observerRouteReceiptOwner) finish(ctx context.Context, findings *[]finding, probeErr error) {
	if self == nil {
		return
	}
	r := &self.receipt
	r.FinishedAt, r.ContextEnded = self.env.now().UTC(), ctx.Err() != nil
	r.PolicyUnchanged = r.PolicySha256 != "" && r.PolicySha256 == observerRoutePolicyDigest(observerRouteEffectivePolicy(self.env.cfg, self.excluded, self.settings), self.settings.MaxReceiptBytes)
	r.PolicyAssociationComplete = r.PolicyAssociationComplete && r.PolicyUnchanged
	r.Complete = len(r.Targets) > 0 && r.TargetsOmitted == 0 && r.CycleId != "" && r.SourceAssociationComplete && r.PolicyAssociationComplete && r.Mode == string(addressModeOverlay) && !r.ContextEnded && probeErr == nil && !r.FinishedAt.Before(r.StartedAt)
	for i := range r.Targets {
		if r.ContextEnded {
			r.Targets[i].Complete = false
		}
		r.Complete = r.Complete && r.Targets[i].Complete
	}
	if r.ContextEnded {
		r.Reason = "context-ended"
	} else if r.Complete {
		r.Reason = "observed"
	} else if r.Reason == "not-observed" {
		r.Reason = "incomplete"
	}
	sha, err := self.store()
	if self.claimed {
		status := "stored"
		if err != nil {
			status = "unavailable"
		}
		self.association.completion.finish(sha, status)
	}
	if err != nil {
		// A sink error must not discard a real diverted/absent finding.
		*findings = append(*findings, cannotObserveFinding("monitor-host/observer-route-receipt", errors.New("private observer route receipt unavailable")))
		return
	}
	for i := range *findings {
		if !(*findings)[i].healthy {
			(*findings)[i].evidence += " Private observer_route_receipt_sha256=" + sha
		}
	}
}

func (self *observerRouteReceiptOwner) store() (string, error) {
	if self.receipt.CycleId == "" || self.settings.MaxReceiptBytes <= 0 || self.settings.MaxReceiptTargets <= 0 {
		return "", errors.New("observer route receipt policy unavailable")
	}
	raw, err := json.MarshalIndent(self.receipt, "", "  ")
	if err != nil || len(raw)+1 > self.settings.MaxReceiptBytes {
		return "", errors.New("observer route receipt exceeds bound")
	}
	dir := filepath.Join(self.env.cfg.stateDir, "observer-route")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", errors.New("observer route receipt directory unavailable")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", fmt.Errorf("observer route receipt directory not private")
	}
	return pgSampleStoreObject(dir, "receipt-", append(raw, '\n'))
}
