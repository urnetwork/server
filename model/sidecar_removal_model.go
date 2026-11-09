package model

// §7.10's removal effects chain (I-15, owner decision 36): what api ends in the
// same write as a removal or deactivation. Each area registers its own effect,
// tokens, keys, auth codes, listeners, publishes, certificates, DKIM selectors,
// TLSA records and push-token tombstones among them, with
// RegisterRemovalEffect, and ApplyRemovalEffectsInTx runs every one in the
// deactivation's own transaction, so a removal that commits has ended them all
// and one that rolls back has ended none.
//
// Every effect is idempotent: §19.10's restore applies each removal recorded
// after the main dump's snapshot again, before the token exchange reopens, and a
// removal may reach a subject twice, a child with its parent's removal and with
// its own. An effect that fails fails the removal's transaction.
//
// The chain's own effect, "rev", owes §7.8's rev/ objects (sidecar_rev_model.go):
// one per removed sidecar or member, and one per removed client that is bound
// to a sidecar (sidecar_identity_binding), since only such a client can hold a
// service token: the exchange takes only a bound key's proof. A client of a
// binary that does not link MG1 has no binding, so it owes none.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// One subject a removal ends: a client, a sidecar or a member of a network.
type RemovalSubject struct {
	NetworkId server.Id
	Kind      sidecar.RevocationKind
	SubjectId server.Id
}

// One removal or deactivation, as the effects see it.
type Removal struct {
	// What it ends: every client it deactivated, the descendants with their
	// parent, and the sidecars and members it revoked.
	Subjects []*RemovalSubject
	// Why, as rev/ records it.
	Reason sidecar.RevocationReason
	// When it took effect: the not_before of the objects it owes is the whole
	// second after this and SidecarRevSettings.NotBeforeMargin.
	Time time.Time
}

// The subjects of one kind.
func (self *Removal) SubjectsOf(kind sidecar.RevocationKind) []*RemovalSubject {
	subjects := []*RemovalSubject{}
	for _, subject := range self.Subjects {
		if subject.Kind == kind {
			subjects = append(subjects, subject)
		}
	}
	return subjects
}

// A removal's clients, each with its network: a deactivation of clientIds of
// networkId.
func NewClientRemoval(networkId server.Id, clientIds []server.Id, reason sidecar.RevocationReason, at time.Time) *Removal {
	subjects := make([]*RemovalSubject, 0, len(clientIds))
	for _, clientId := range clientIds {
		subjects = append(subjects, &RemovalSubject{NetworkId: networkId, Kind: sidecar.RevocationKindClient, SubjectId: clientId})
	}
	return &Removal{Subjects: subjects, Reason: reason, Time: at}
}

// One area's part of a removal, run in the removal's transaction. It must be
// idempotent (the file header), must not begin another transaction or take
// another Postgres connection, and must not do store or network I/O, which
// cannot join the transaction: such work is owed, as rev/'s is, through a task
// the transaction schedules.
type RemovalEffect func(ctx context.Context, tx server.PgTx, removal *Removal) error

var removalEffectsLock sync.Mutex
var removalEffects = map[string]RemovalEffect{}

// Adds name's effect to every removal from now on, the one hook each area uses
// rather than growing the deactivation (§7.10). A later effect of the same name
// replaces the earlier. Effects run in name order.
func RegisterRemovalEffect(name string, effect RemovalEffect) {
	if name == "" || effect == nil {
		panic("a removal effect needs a name and a function")
	}
	removalEffectsLock.Lock()
	defer removalEffectsLock.Unlock()
	removalEffects[name] = effect
}

// Removes name's effect, for a test that registered one.
func Testing_UnregisterRemovalEffect(name string) {
	removalEffectsLock.Lock()
	defer removalEffectsLock.Unlock()
	delete(removalEffects, name)
}

// Runs every registered effect of removal in tx, the deactivation's own
// transaction, in name order. The first that fails stops the rest and fails the
// transaction.
func ApplyRemovalEffectsInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	if removal == nil || len(removal.Subjects) == 0 {
		return nil
	}
	effects := func() []string {
		removalEffectsLock.Lock()
		defer removalEffectsLock.Unlock()
		names := make([]string, 0, len(removalEffects))
		for name := range removalEffects {
			names = append(names, name)
		}
		slices.Sort(names)
		return names
	}()
	for _, name := range effects {
		effect := func() RemovalEffect {
			removalEffectsLock.Lock()
			defer removalEffectsLock.Unlock()
			return removalEffects[name]
		}()
		if effect == nil {
			continue
		}
		if err := effect(ctx, tx, removal); err != nil {
			return fmt.Errorf("removal effect %s: %w", name, err)
		}
	}
	return nil
}

// The name the chain's own rev/ effect registers under.
const RemovalEffectRev = "rev"

func init() {
	RegisterRemovalEffect(RemovalEffectRev, applyRevocationEffectInTx)
}

// Owes a rev/ object for each sidecar and member the removal ends, and for each
// client it ends that is bound to a sidecar.
func applyRevocationEffectInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	if removal.Time.IsZero() {
		return errors.New("a removal with no time")
	}
	notBefore := SidecarRevocationNotBefore(removal.Time, DefaultSidecarRevSettings())
	revocations := []*SidecarRevocation{}
	owe := func(subject *RemovalSubject) {
		revocations = append(revocations, &SidecarRevocation{
			NetworkId: subject.NetworkId,
			Kind:      subject.Kind,
			SubjectId: subject.SubjectId,
			NotBefore: notBefore,
			Reason:    removal.Reason,
		})
	}

	clients := removal.SubjectsOf(sidecar.RevocationKindClient)
	if 0 < len(clients) && server.SidecarMg1Linked() {
		clientIds := make([]server.Id, 0, len(clients))
		for _, client := range clients {
			clientIds = append(clientIds, client.SubjectId)
		}
		boundClientIds := map[server.Id]bool{}
		result, err := tx.Query(
			ctx,
			`
				SELECT client_id
				FROM sidecar_identity_binding
				WHERE client_id = ANY($1)
			`,
			clientIds,
		)
		if err != nil {
			return err
		}
		for result.Next() {
			var clientId server.Id
			if err := result.Scan(&clientId); err != nil {
				result.Close()
				return err
			}
			boundClientIds[clientId] = true
		}
		result.Close()
		if err := result.Err(); err != nil {
			return err
		}
		for _, client := range clients {
			if boundClientIds[client.SubjectId] {
				owe(client)
			}
		}
	}
	for _, kind := range []sidecar.RevocationKind{sidecar.RevocationKindSidecar, sidecar.RevocationKindMember} {
		for _, subject := range removal.SubjectsOf(kind) {
			owe(subject)
		}
	}
	WriteRevocations(ctx, tx, revocations)
	return nil
}
