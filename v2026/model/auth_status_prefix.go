package model

import "regexp"

// The router turns a leading "<4xx|5xx> " on an error message into the HTTP
// status and drops it from the body (router.RaiseHttpError). That convention
// only works for refusals that travel as a Go error.
//
// /auth/add-auth and /auth/remove-auth deliberately do NOT travel that way:
// their refusals are spec'd response objects (AddAuthResult.error /
// RemoveAuthResult.error in connect/api/bringyour.yml), answered with HTTP 200
// so the structured field stays reachable. Those two shapes meet wherever a
// helper shared with a status-carrying path -- validateWalletAuth, which
// NetworkCreate also calls -- hands back an already-prefixed message. Copied
// verbatim into error.message, the literal digits render in the client's error
// toast.
//
// PeelStatusPrefix is the boundary between the two conventions: a message on
// its way into a 200 body loses the prefix it would have spent on a status.
// The grammar is deliberately identical to router.httpErrorCodeRegex, tags
// included, so a message the router would have peeled is peeled here too and
// the two cannot drift into disagreeing about what a prefix is. It is
// duplicated rather than imported to keep the existing import direction:
// model does not import the router.
var statusPrefixRegex = regexp.MustCompile(`^((?:\[[^\]\r\n]*\])*)[ \t]*([45][0-9]{2})[ \t]+(?s:(.*))$`)

// PeelStatusPrefix returns message without the leading router status prefix,
// or message unchanged when it carries none.
func PeelStatusPrefix(message string) string {
	if groups := statusPrefixRegex.FindStringSubmatch(message); groups != nil {
		return groups[3]
	}
	return message
}
