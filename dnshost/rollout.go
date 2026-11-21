package dnshost

import (
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/urnetwork/sidecar/dnsservice"
)

func (self *EdgeHost) rolloutStatusHeaders(headers http.Header, request *http.Request) {
	probe := request.Header.Get(dnsservice.DnsRolloutProofHeader)
	if probe == "" {
		return
	}
	headers.Set("Cache-Control", "no-store")
	nonceText, root, ok := strings.Cut(probe, " ")
	if !ok {
		return
	}
	var challenge *dnsservice.DnsRolloutNonce
	if nonceText != dnsservice.DnsCanaryLabel {
		var nonce dnsservice.DnsRolloutNonce
		if len(nonceText) != hex.EncodedLen(len(nonce)) {
			return
		}
		if _, err := hex.Decode(nonce[:], []byte(nonceText)); err != nil {
			return
		}
		challenge = &nonce
	}
	if proof, serial := self.edge.CachedRolloutProof(root, challenge); proof != "" {
		headers.Set(dnsservice.DnsRolloutProofHeader, strconv.FormatUint(uint64(serial), 10)+" "+proof)
	}
}
