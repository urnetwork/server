package dnshost

// dnsservice.DnsChallengeWriter over model.DnsPutChallenge (DNS-M5-13): certs'
// DNS-01 writes through §12.6's one write path, with the model's refusals read
// as the route answers them, the hourly cap's with how long until the name
// takes a write again, which the route's 429 names in its Retry-After
// (QD-1204). Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/model"
)

var _ dnsservice.DnsChallengeWriter = (*Challenges)(nil)

// The writer. It holds nothing.
type Challenges struct{}

func NewChallenges() *Challenges {
	return &Challenges{}
}

// Writes values at name's challenge in root, or removes them with none: a
// name or values the path refuses, a suspended zone included, is
// dnsservice.ErrDnsChallengeRefused, and the hourly cap a
// *dnsservice.DnsChallengeRateError carrying the model's wait. A restore's
// refusal and a removal of nothing pass as they are.
func (self *Challenges) PutChallenge(ctx context.Context, root string, name string, values []string, ttl uint32) (serial uint64, returnErr error) {
	returnErr = storeCall(func() error {
		result, err := model.DnsPutChallenge(ctx, &model.DnsChallengeArgs{Root: root, Name: name, Values: values, Ttl: ttl})
		switch {
		case err == nil:
			serial = result.Serial
			return nil
		case errors.Is(err, model.ErrDnsChallengeRate):
			rateErr := &dnsservice.DnsChallengeRateError{Err: err}
			var modelRateErr *model.DnsChallengeRateError
			if errors.As(err, &modelRateErr) {
				rateErr.RetryAfter = modelRateErr.RetryAfter
			}
			return rateErr
		case errors.Is(err, model.ErrDnsChallengeName), errors.Is(err, model.ErrDnsChallengeValues),
			errors.Is(err, model.ErrDnsZoneSuspended), errors.Is(err, model.ErrDnsHostSuspended),
			errors.Is(err, model.ErrDnsOwnerOutsideZone), errors.Is(err, dnsservice.ErrDnsZoneLint):
			return fmt.Errorf("%w: %w", dnsservice.ErrDnsChallengeRefused, err)
		}
		return err
	})
	return
}
