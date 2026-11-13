package controller

// st_ctf_amendment.go — E_c and the capture the flag terms (sn
// CAPTURETHEFLAG.md §2.4): the signed settlement amendment entry v2 in force
// for an epoch, read with sn/amendment. A deployment with a pinned signer reads
// the published file (amendment.URL) verified against it and keeps the last
// verified copy in the blob store, so a restart does not depend on the site; one
// without (testnet) has no signed entries, and only the configured lines stand
// in (st.yml ctf.amendment_lines). The entries in force being unknown is never
// read as "not in force": a live step that cannot tell waits and alerts.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urfoundation/sn/v2026/amendment"
	"github.com/urfoundation/sn/v2026/ctf"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// stCtfAmendmentRefreshInterval spaces the fetches of the published file; in
// between, the last verified file is read. After a fetch that failed, the
// next is tried sooner.
const (
	stCtfAmendmentRefreshInterval      = 10 * time.Minute
	stCtfAmendmentProblemRetryInterval = 1 * time.Minute
)

// errStCtfTerms is an entry in force whose terms package ctf cannot build, or
// whose carry is not the configured one.
var errStCtfTerms = errors.New("st ctf: the amendment entry's terms are not the ones this server builds")

// The seams tests replace: the pinned signers, the published address and the
// store of the last verified file.
var (
	stCtfAmendmentSigner = amendment.SignerFor
	stCtfAmendmentUrl    = amendment.URL
	stCtfAmendmentStorer = func(cfg *StConfig) amendment.Store {
		return &stCtfAmendmentStore{key: fmt.Sprintf("%d-%s.yml", cfg.ChainId, strings.ToLower(cfg.ContractAddress.Hex()))}
	}
)

// stCtfAmendmentStore keeps a deployment's last verified amendment file in
// the blob store beside the ledger objects. A read or write failure is an
// error, which the source reports as a problem and works around.
type stCtfAmendmentStore struct {
	key string
}

func (self *stCtfAmendmentStore) path(store server.BlobStore) string {
	return filepath.ToSlash(filepath.Join(store.Prefix(), "st", "ctf", "amendments", self.key))
}

func (self *stCtfAmendmentStore) Load(ctx context.Context) ([]byte, error) {
	store, err := stLedgerV2Store()
	if err != nil {
		return nil, err
	}
	reader, err := store.Get(ctx, self.path(store))
	if server.IsBlobNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(reader, 1<<20))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		if server.IsBlobNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return raw, nil
}

func (self *stCtfAmendmentStore) Save(ctx context.Context, raw []byte) error {
	store, err := stLedgerV2Store()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "urnetwork-st-ctf-amendments-*.yml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.Write(raw)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	return store.Put(ctx, self.path(store), tmp.Name(), "application/yaml")
}

// stCtfAmendmentHolder is one deployment's source, the file last read from
// it, and whether that read could not take the published file.
type stCtfAmendmentHolder struct {
	source    *amendment.Source
	lock      sync.Mutex
	file      *amendment.File
	problem   error
	refreshed time.Time
}

var stCtfAmendmentHolders sync.Map // deployment key → *stCtfAmendmentHolder

// stCtfAmendmentFile returns the verified amendment file of the deployment:
// the published one, refreshed at most every stCtfAmendmentRefreshInterval,
// else the last verified copy, else nil (unknown). stale reports a last copy
// served because the published file could not be taken: it may predate an
// entry. A deployment without a pinned signer has the empty unpublished file.
func stCtfAmendmentFile(ctx context.Context, cfg *StConfig) (file *amendment.File, stale bool, err error) {
	netuid := uint16(cfg.Netuid)
	signer, pinned := stCtfAmendmentSigner(cfg.ChainId, netuid)
	if !pinned {
		file, err = amendment.Unpublished(cfg.ChainId, netuid)
		return file, false, err
	}
	value, _ := stCtfAmendmentHolders.LoadOrStore(string(cfg.DeploymentKey()), &stCtfAmendmentHolder{
		source: amendment.NewSource(stCtfAmendmentUrl, cfg.ChainId, netuid, signer, stCtfAmendmentStorer(cfg)),
	})
	holder := value.(*stCtfAmendmentHolder)
	holder.lock.Lock()
	defer holder.lock.Unlock()
	interval := stCtfAmendmentRefreshInterval
	if holder.problem != nil {
		interval = stCtfAmendmentProblemRetryInterval
	}
	if holder.file != nil && stCtfServerNow().Sub(holder.refreshed) < interval {
		return holder.file, holder.problem != nil, nil
	}
	file, problem := holder.source.Refresh(ctx)
	if problem != nil {
		stCtfAlert(cfg, StCtfAlertAmendment, "file", "%s", problem)
		glog.Infof("[st]ctf amendment file problem: %s\n", problem)
	}
	if file == nil {
		return nil, false, fmt.Errorf("%w: %v", amendment.ErrUnknown, problem)
	}
	holder.file, holder.problem, holder.refreshed = file, problem, stCtfServerNow()
	return file, problem != nil, nil
}

// stCtfInForce returns the capture the flag entry in force for an epoch, nil
// when none is (the epoch is before E_c), after checking its terms. An
// unknown file, or a last copy without the entry while the published file
// cannot be read, is an error wrapping amendment.ErrUnknown.
func stCtfInForce(ctx context.Context, cfg *StConfig, epoch uint64) (*amendment.Entry, error) {
	file, stale, err := stCtfAmendmentFile(ctx, cfg)
	if err != nil {
		return nil, err
	}
	fallback, err := amendment.ParseFallback(cfg.ChainId, uint16(cfg.Netuid), cfg.Ctf.AmendmentLines)
	if err != nil {
		return nil, err
	}
	in, err := file.At(cfg.NoId, epoch, fallback)
	if err != nil {
		return nil, err
	}
	entry := in.CaptureTheFlag
	if entry == nil && stale {
		// An older copy may predate the entry: unknown, never "not in force".
		return nil, fmt.Errorf("%w: the published file could not be read, and the last verified copy has no capture the flag entry for epoch %d", amendment.ErrUnknown, epoch)
	}
	if entry == nil {
		return nil, nil
	}
	if err := stCtfCheckTerms(cfg, entry); err != nil {
		stCtfAlert(cfg, StCtfAlertAmendment, fmt.Sprintf("epoch %d", epoch), "%s", err)
		return nil, err
	}
	return entry, nil
}

// stCtfCheckTerms refuses an entry whose terms package ctf does not build, or
// whose carry differs from the configured carry. Only chain, sn, no, the first
// epoch and the carry vary between valid lines (sn/amendment), so these checks
// guard a later grammar against a server that predates it.
func stCtfCheckTerms(cfg *StConfig, entry *amendment.Entry) error {
	terms := entry.Ctf
	switch {
	case terms == nil:
		return fmt.Errorf("%w: the entry has no capture the flag terms", errStCtfTerms)
	case entry.NoID != cfg.NoId || terms.CtfPool != ctf.PoolID(cfg.NoId):
		return fmt.Errorf("%w: no %d, ctf pool %d", errStCtfTerms, entry.NoID, terms.CtfPool)
	case terms.ProviderSlots != ctf.Ranks || terms.ExtenderSlots != ctf.Ranks:
		return fmt.Errorf("%w: slots %d providers %d extenders", errStCtfTerms, terms.ProviderSlots, terms.ExtenderSlots)
	case terms.Countries != ctf.NumCountries:
		return fmt.Errorf("%w: %d countries", errStCtfTerms, terms.Countries)
	case terms.Presence != (amendment.Ratio{Num: 1, Den: 2}):
		return fmt.Errorf("%w: presence %d/%d", errStCtfTerms, terms.Presence.Num, terms.Presence.Den)
	case terms.Units != amendment.UnitsEgressPrefix:
		return fmt.Errorf("%w: units %q", errStCtfTerms, terms.Units)
	case terms.DrawDelay != ctf.DrawDelay*time.Second:
		return fmt.Errorf("%w: draw delay %s", errStCtfTerms, terms.DrawDelay)
	case terms.Carry == (common.Address{}):
		return fmt.Errorf("%w: no carry contract", errStCtfTerms)
	case cfg.Ctf.CarryAddress != (common.Address{}) && cfg.Ctf.CarryAddress != terms.Carry:
		return fmt.Errorf("%w: carry %s, configured %s", errStCtfTerms, terms.Carry.Hex(), cfg.Ctf.CarryAddress.Hex())
	}
	return nil
}

// stCtfCarryAddress is the carry contract for an epoch: the entry's when one is
// in force, else the configured one (shadow before the entry), else zero.
func stCtfCarryAddress(cfg *StConfig, entry *amendment.Entry) common.Address {
	if entry != nil && entry.Ctf != nil {
		return entry.Ctf.Carry
	}
	return cfg.Ctf.CarryAddress
}
