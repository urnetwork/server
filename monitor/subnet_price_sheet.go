// The signed demand deposit price, as the subnet readers see it. The sheet is
// fetched here, from its published address, and verified here against the
// deployment's pinned signer: a producer's own stored copy is not evidence of what
// validators read. Each probe holds the last sheet that verified, so an outage or a
// tampered file leaves the rates in force what they were. A deployment with no
// pinned signer fetches nothing, and its reviewed policy document stays in force.
package monitor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/urfoundation/sn/demand/pricesheet"
	"github.com/urfoundation/sn/protocol"
)

const (
	// A published file is bounded before it is parsed; the parser's own bound is
	// the same.
	maximumSubnetPriceSheetBytes = 256 << 10
	// How long the published file may go unread before the sheet is stale. Nobody's
	// deposit stops in that time, but a new entry would reach nobody.
	subnetPriceSheetStaleAfter = 6 * time.Hour
)

// The pinned signers and the published address. Tests point them at their own key
// and route.
var (
	subnetPriceSheetSigner = pricesheet.SignerFor
	subnetPriceSheetUrl    = pricesheet.URL
)

// The last sheet that verified for one probe, and when.
type subnetPriceSheetMemory struct {
	stateLock sync.Mutex
	sheet     *pricesheet.Sheet
	at        time.Time
}

func (self *subnetPriceSheetMemory) last() (*pricesheet.Sheet, time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.sheet, self.at
}

func (self *subnetPriceSheetMemory) remember(sheet *pricesheet.Sheet, at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.sheet, self.at = sheet, at
}

// One read of the sheet. Sheet is the one to price with: the published file when it
// verified, otherwise the last that did, and nil when nothing has. Reason is the fixed
// public cause when the published file was not taken. Bad says that a file was served
// and fails verification, as opposed to no file being available.
type subnetPriceSheetRead struct {
	pinned       bool
	sheet        *pricesheet.Sheet
	lastVerified time.Time
	reason       string
	bad          bool
}

// Reads the published file. A deployment with no pinned signer reads nothing.
func (self *subnetPriceSheetMemory) read(ctx context.Context, run *subnetRun) (subnetPriceSheetRead, error) {
	deployment := run.settings.Deployment
	signer, pinned := subnetPriceSheetSigner(deployment.ChainId, deployment.Netuid)
	if !pinned {
		return subnetPriceSheetRead{}, nil
	}
	last, at := self.last()
	result := subnetPriceSheetRead{pinned: true, sheet: last, lastVerified: at}
	raw, err := readSubnetPriceSheetFile(ctx, run)
	if canceled := subnetCanceled(ctx); canceled != nil {
		return subnetPriceSheetRead{}, canceled
	}
	if err != nil {
		result.reason = "sheet-unavailable"
		return result, nil
	}
	next, err := pricesheet.Parse(raw)
	if err != nil {
		result.reason, result.bad = "sheet-unparseable", true
		return result, nil
	}
	if err := next.Verify(deployment.ChainId, deployment.Netuid, signer); err != nil {
		result.reason, result.bad = "sheet-unverified", true
		return result, nil
	}
	if !pricesheet.Extends(next, last) {
		result.reason, result.bad = "sheet-rollback", true
		return result, nil
	}
	result.sheet, result.lastVerified = next, run.started
	run.afterPublish(func() { self.remember(next, run.started) })
	return result, nil
}

// One bounded Get of the published file through the readers' Do seam, which refuses
// a redirect: the published address is the only source.
func readSubnetPriceSheetFile(parent context.Context, run *subnetRun) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, run.limits.requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, subnetPriceSheetUrl, nil)
	if err != nil {
		return nil, subnetReadFailure("request-invalid", err)
	}
	request.Header.Set("Accept", "application/yaml, text/plain")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Cache-Control", "no-cache, no-store")
	response, err := doScopedGrafanaRequest(run.session.client, request)
	if response == nil || response.Body == nil {
		return nil, subnetReadFailure("sheet-unavailable", errors.Join(err, ctx.Err()))
	}
	defer response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		return nil, subnetReadFailure("sheet-unavailable", err)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumSubnetPriceSheetBytes+1))
	if err != nil {
		return nil, subnetReadFailure("sheet-unavailable", err)
	}
	return raw, nil
}

// Whether a signed price is in force for an epoch under base, or scheduled after it.
func subnetDepositPriced(sheet *pricesheet.Sheet, epoch uint64, base protocol.DepositPolicy) (inForce bool, scheduled bool) {
	inForce = !pricesheet.Effective(sheet, epoch, base).IsZeroPrice()
	if sheet != nil {
		for i := range sheet.Entries {
			if epoch < sheet.Entries[i].EffectiveEpoch && !sheet.Entries[i].DepositPolicy(base).IsZeroPrice() {
				scheduled = true
			}
		}
	}
	return inForce, scheduled
}
