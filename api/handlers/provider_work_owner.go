// SDK generation intake uses the same finite route-set owner as request/cut
// transport. Public retrieval exposes original signatures, never a live roster.
package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// One optional generation selector chooses exact original bytes. Omitting it
// returns a bounded historical index for an independently selected client/key.
func (self *ProviderWorkHandlers) serveOwner(ctx context.Context, w http.ResponseWriter, r *http.Request, domain [32]byte) {
	if r.Method == http.MethodGet {
		names := []string{"domain", "client", "key"}
		_, exact := r.URL.Query()["generation"]
		if exact {
			names = append(names, "generation")
		}
		query, err := providerWorkQuery(r, names)
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		var owner model.ProviderWorkOwner
		for _, field := range []struct {
			value  string
			output []byte
		}{{value: query["domain"], output: owner.DomainHash[:]}, {value: query["client"], output: owner.ClientId[:]}, {value: query["key"], output: owner.PublicKey[:]}} {
			if err := providerWorkSelector(field.value, field.output); err != nil {
				providerWorkHttpError(w, err)
				return
			}
		}
		if owner.DomainHash != domain || exact && providerWorkSelector(query["generation"], owner.Generation[:]) != nil {
			providerWorkHttpError(w, model.ErrProviderWorkInvalid)
			return
		}
		if exact {
			raw, err := model.GetProviderWorkOwner(ctx, owner)
			if err != nil {
				providerWorkHttpError(w, err)
				return
			}
			digest := sha256.Sum256(raw)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"`+hex.EncodeToString(digest[:])+`"`)
			w.Header().Set("Cache-Control", "public, immutable, max-age=31536000")
			_, _ = w.Write(raw)
			return
		}
		index, err := model.ListProviderWorkOwners(ctx, owner)
		if err != nil {
			providerWorkHttpError(w, err)
			return
		}
		providerWorkJson(w, index)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Provider work route unavailable.", http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength > protocol.MaximumOriginalWorkOwnerBytes {
		http.Error(w, "Provider work body exceeds capacity.", http.StatusRequestEntityTooLarge)
		return
	}
	if r.URL.RawQuery != "" || r.Body == nil || r.ContentLength <= 0 || r.Header.Get("Content-Type") != "application/json" || len(r.Header.Values("Content-Type")) != 1 || len(r.Header.Values("Content-Encoding")) != 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
		providerWorkHttpError(w, model.ErrProviderWorkInvalid)
		return
	}
	selectedDomain, rootSigner, err := controller.LoadProviderWorkOwnerPolicy()
	if err != nil {
		providerWorkHttpError(w, err)
		return
	}
	if selectedDomain != domain {
		providerWorkHttpError(w, model.ErrProviderWorkConflict)
		return
	}
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		http.Error(w, "Provider work body owner unavailable.", http.StatusServiceUnavailable)
		return
	}
	body, err := ownSnAttemptUploadBody(ctx, w, r.Body)
	if err != nil {
		providerWorkHttpError(w, err)
		return
	}
	raw, err := readProviderWorkBody(ctx, body, r.ContentLength)
	if err != nil {
		providerWorkHttpError(w, err)
		return
	}
	receipt, err := model.RetainProviderWorkOwner(ctx, raw, domain, rootSigner)
	if err != nil {
		providerWorkHttpError(w, err)
		return
	}
	providerWorkJson(w, receipt)
}
