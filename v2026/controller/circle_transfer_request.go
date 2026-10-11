// A caller intent owns one durable request. Unknown responses reconcile via
// Circle's same-key response contract; known challenges use GET exclusively.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

var ErrCircleTransferRequestId = errors.New("customer transfer requires a caller-retained request_id; reuse it for every retry")
var ErrCircleTransferUnknown = errors.New("customer challenge outcome is unknown; original request is retained, retry only with the same request_id")

type circleTransferClientKey struct{}

// Immutable request-owner hooks use the real transport and token creator when
// omitted. Tests bind them to one session; unrelated sessions share no state.
type circleTransferClient struct {
	core         CoreCircleApiClient
	tokenSource  func(*session.ClientSession) (*CircleUserToken, error)
	defaultName  string
	defaultChain string
}

func (self *circleTransferClient) transfer(args *WalletCircleTransferOutArgs, owner *session.ClientSession) (*WalletCircleTransferOutResult, error) {
	if err := owner.Ctx.Err(); err != nil {
		return nil, err
	}
	if args == nil || args.RequestId == nil || *args.RequestId == (server.Id{}) {
		return nil, ErrCircleTransferRequestId
	}
	if !args.Terms || args.ToAddress == "" || len(args.ToAddress) > 256 || strings.TrimSpace(args.ToAddress) != args.ToAddress {
		return nil, fmt.Errorf("transfer requires accepted terms and an exact bounded destination")
	}
	if _, err := model.CircleTransferAmount(args.AmountUsdcNanoCents); err != nil {
		return nil, err
	}
	if owner.ByJwt == nil {
		return nil, fmt.Errorf("customer transfer requires an authenticated owner")
	}
	ctx, cancel, err := self.core.beginReads(owner.Ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	borrowed := *owner
	borrowed.Ctx = ctx
	request, err := model.GetCircleTransferRequest(ctx, owner.ByJwt.NetworkId, owner.ByJwt.UserId, *args.RequestId)
	if err != nil {
		return nil, err
	}
	if request != nil && (request.Basis.Destination != args.ToAddress || request.Basis.Amount != args.AmountUsdcNanoCents) {
		return nil, model.ErrCircleTransferRequestChanged
	}
	if request != nil && request.ReviewRequired {
		return nil, model.ErrCircleTransferObservation
	}
	tokenSource := self.tokenSource
	if tokenSource == nil {
		tokenSource = createCircleUserToken
	}
	userToken, err := tokenSource(&borrowed)
	if err != nil {
		return nil, errors.Join(err, ctx.Err())
	}
	if userToken == nil || userToken.circleUserId == (server.Id{}) || userToken.UserToken == "" || len(userToken.UserToken) > 16*1024 || userToken.EncryptionKey == "" || len(userToken.EncryptionKey) > 16*1024 {
		return nil, fmt.Errorf("customer challenge has no bounded user authorization")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request == nil {
		name, chain := self.defaultName, self.defaultChain
		if name == "" || chain == "" {
			name, chain = fmt.Sprint(circleConfig()["blockchain_name"]), fmt.Sprint(circleConfig()["blockchain"])
		}
		wallets, err := self.core.findCircleWallets(&borrowed, func(*session.ClientSession) (*CircleUserToken, error) { return userToken, nil }, name, chain)
		if err != nil {
			return nil, err
		}
		var wallet *CircleWalletInfo
		for _, candidate := range wallets {
			if wallet == nil || wallet.CreateDate.Before(candidate.CreateDate) {
				wallet = candidate
			}
		}
		if wallet == nil || wallet.TokenId == "" {
			return nil, fmt.Errorf("no transferable customer wallet")
		}
		request, err = model.RetainCircleTransferRequest(ctx, owner.ByJwt.NetworkId, owner.ByJwt.UserId, *args.RequestId, model.CircleTransferBasis{
			CircleUserId: userToken.circleUserId, WalletId: wallet.WalletId, TokenId: wallet.TokenId, Blockchain: wallet.Blockchain, Destination: args.ToAddress, Amount: args.AmountUsdcNanoCents})
		if err != nil {
			return nil, err
		}
	}
	if request.Basis.CircleUserId != userToken.circleUserId {
		return nil, model.ErrCircleTransferRequestChanged
	}
	if request.ChallengeId == nil {
		request, err = model.BeginCircleTransferSubmission(ctx, request)
		if err != nil {
			return nil, err
		}
	}
	if request.ChallengeId != nil {
		request, err = self.readChallenge(ctx, request, userToken.UserToken)
	} else {
		request, err = self.submitChallenge(ctx, request, userToken.UserToken)
	}
	if err != nil {
		return nil, err
	}
	if request == nil || request.ChallengeId == nil || request.ReviewRequired {
		return nil, model.ErrCircleTransferObservation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := request.RequestId
	return &WalletCircleTransferOutResult{RequestId: &id, UserToken: userToken, ChallengeId: *request.ChallengeId, ChallengeStatus: request.ChallengeStatus}, nil
}

func circleChallengeId(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

// One explicit submit/reconciliation attempt per public call. There is no
// automatic POST retry, redirect forwarding, fresh key, or unbounded body.
func (self *circleTransferClient) submitChallenge(ctx context.Context, request *model.CircleTransferRequest, userToken string) (*model.CircleTransferRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	attempt, cancel := context.WithTimeout(ctx, server.DefaultHttpTimeout)
	defer cancel()
	call, err := http.NewRequestWithContext(attempt, http.MethodPost, "https://api.circle.com/v1/w3s/user/transactions/transfer", strings.NewReader(request.Body))
	if err != nil {
		return nil, err
	}
	self.core.readHeaders(userToken)(call.Header)
	call.Header.Set("Content-Type", "application/json")
	scope := ctx.Value(paymentReadScopeKey{}).(*paymentReadScope)
	response, transportErr := scope.hooks.do(call)
	var body []byte
	var readErr, closeErr error
	status := 0
	if response != nil {
		status = response.StatusCode
		if response.Body != nil {
			body, readErr = io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
			closeErr = response.Body.Close()
		} else {
			readErr = io.ErrUnexpectedEOF
		}
	}
	var challengeId string
	var observationErr error
	if transportErr == nil && readErr == nil {
		if len(body) > 64*1024 {
			observationErr = fmt.Errorf("customer challenge response exceeds capacity")
		} else if status != http.StatusOK && status != http.StatusCreated {
			observationErr = &server.HttpStatusError{StatusCode: status, Status: http.StatusText(status)}
		} else {
			var value struct {
				Data struct {
					ChallengeId string `json:"challengeId"`
				} `json:"data"`
			}
			if err := json.Unmarshal(body, &value); err != nil {
				observationErr = err
			} else if !circleChallengeId(value.Data.ChallengeId) {
				observationErr = model.ErrCircleTransferObservation
			} else {
				challengeId = value.Data.ChallengeId
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(ErrCircleTransferUnknown, transportErr, readErr, closeErr, observationErr, err)
	}
	digest := sha256.Sum256(body)
	challengeStatus := "UNKNOWN"
	if challengeId != "" {
		challengeStatus = "PENDING"
	}
	retained, journalErr := model.ObserveCircleTransfer(ctx, request, challengeId, challengeStatus, hex.EncodeToString(digest[:]), status)
	if err := errors.Join(transportErr, readErr, closeErr, observationErr, journalErr, attempt.Err()); err != nil || challengeId == "" {
		return nil, errors.Join(ErrCircleTransferUnknown, err)
	}
	return retained, nil
}

// A known challenge never falls back to a write on an outage, 404, changed
// identity, or cancellation. COMPLETE describes the challenge, not payment.
func (self *circleTransferClient) readChallenge(ctx context.Context, request *model.CircleTransferRequest, userToken string) (*model.CircleTransferRequest, error) {
	id := *request.ChallengeId
	if !circleChallengeId(id) {
		return nil, model.ErrCircleTransferObservation
	}
	type observed struct{ id, status, digest string }
	value, err := paymentHttpGet(ctx, self.core.readSettings, self.core.readHooks, "https://api.circle.com/v1/w3s/user/challenges/"+url.PathEscape(id), self.core.readHeaders(userToken), func(_ *http.Response, body []byte) (observed, error) {
		data, err := circleReadData(body, "challenge")
		if err != nil {
			return observed{}, err
		}
		var challenge struct {
			Id     string `json:"id"`
			Type   string `json:"type"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(data, &challenge); err != nil {
			return observed{}, err
		}
		if challenge.Id != id || challenge.Type != "CREATE_TRANSACTION" {
			return observed{}, model.ErrCircleTransferObservation
		}
		switch challenge.Status {
		case "PENDING", "IN_PROGRESS", "COMPLETE", "FAILED", "EXPIRED":
		default:
			return observed{}, model.ErrCircleTransferObservation
		}
		digest := sha256.Sum256(body)
		return observed{id: challenge.Id, status: challenge.Status, digest: hex.EncodeToString(digest[:])}, nil
	})
	if err != nil {
		return nil, err
	}
	return model.ObserveCircleTransfer(ctx, request, value.id, value.status, value.digest, http.StatusOK)
}
