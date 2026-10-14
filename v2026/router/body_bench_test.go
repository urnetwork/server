package router

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

type benchInput struct {
	UserAuth    string `json:"user_auth"`
	Password    string `json:"password"`
	Code        string `json:"code"`
	NetworkName string `json:"network_name"`
	Terms       bool   `json:"terms"`
}

var benchBody = []byte(`{"user_auth":"someone@example.com","password":"hunter2hunter2hunter2","code":"ABCD-1234-EFGH-5678","network_name":"my-network-name","terms":true}`)

// BenchmarkBody_Current mirrors wrapWithInput: ReadAll, the always-evaluated
// glog string copy, then decode via NewDecoder(bytes.NewReader(...)).
func BenchmarkBody_Current(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body := io.Reader(bytes.NewReader(benchBody))
		bodyBytes, _ := io.ReadAll(body)

		// this is what glog.V(2).Infof(...) eagerly evaluates every request:
		logSink = strings.ReplaceAll(string(bodyBytes), "\n", "")

		var input benchInput
		_ = json.NewDecoder(bytes.NewReader(bodyBytes)).Decode(&input)
	}
}

// BenchmarkBody_Lean drops the eager log string and decodes via Unmarshal.
func BenchmarkBody_Lean(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body := io.Reader(bytes.NewReader(benchBody))
		bodyBytes, _ := io.ReadAll(body)

		var input benchInput
		_ = json.Unmarshal(bodyBytes, &input)
	}
}

var logSink string
