package providertunnel

import (
	"encoding/base64"
	"fmt"
	"golang.org/x/net/dns/dnsmessage"
	"net/http"
)

func routeWaitDnsResponse(request *http.Request, empty bool) ([]byte, error) {
	wire, err := base64.RawURLEncoding.DecodeString(request.URL.Query().Get("dns"))
	if err != nil {
		return nil, err
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(wire)
	if err != nil {
		return nil, err
	}
	question, err := parser.Question()
	if err != nil {
		return nil, err
	}
	if question.Type != dnsmessage.TypeA || question.Name.String() != "sample.example." {
		return nil, fmt.Errorf("unexpected fixed DNS question")
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true})
	if err := builder.StartQuestions(); err != nil {
		return nil, err
	}
	if err := builder.Question(question); err != nil {
		return nil, err
	}
	if err := builder.StartAnswers(); err != nil {
		return nil, err
	}
	if !empty {
		if err := builder.AResource(dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: [4]byte{198, 51, 100, 91}}); err != nil {
			return nil, err
		}
	}
	return builder.Finish()
}
