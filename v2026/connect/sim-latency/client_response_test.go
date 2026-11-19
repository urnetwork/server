package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (self roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

type scriptedReadCloser struct {
	chunks   [][]byte
	finalErr error
	closed   bool
}

func (self *scriptedReadCloser) Read(p []byte) (int, error) {
	if len(self.chunks) == 0 {
		return 0, self.finalErr
	}
	chunk := self.chunks[0]
	n := copy(p, chunk)
	if n == len(chunk) {
		self.chunks = self.chunks[1:]
	} else {
		self.chunks[0] = chunk[n:]
	}
	return n, nil
}

func (self *scriptedReadCloser) Close() error {
	self.closed = true
	return nil
}

func testSiteResponse(status int, body string, contentLength int64) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: contentLength,
	}
}

func TestReadSiteResponseComplete(t *testing.T) {
	body := "{\"urls\":[\"/child\"],\"size\":4}\nabcd"
	result, err := readSiteResponse(testSiteResponse(http.StatusOK, body, int64(len(body))))
	if err != nil {
		t.Fatalf("complete response: %v", err)
	}
	if !result.complete {
		t.Fatal("complete response marked incomplete")
	}
	if result.receivedBytes != int64(len(body)) {
		t.Fatalf("received bytes = %d, want %d", result.receivedBytes, len(body))
	}
	if len(result.page.Urls) != 1 || result.page.Urls[0] != "/child" || result.page.Size != 4 {
		t.Fatalf("page = %+v", result.page)
	}
}

func TestReadSiteResponseTruncatedHeader(t *testing.T) {
	body := "{\"urls\":[]"
	result, err := readSiteResponse(testSiteResponse(http.StatusOK, body, int64(len(body))))
	if err == nil || result.complete {
		t.Fatalf("truncated header returned complete=%t err=%v", result.complete, err)
	}
	if result.receivedBytes != int64(len(body)) {
		t.Fatalf("received bytes = %d, want %d", result.receivedBytes, len(body))
	}
}

func TestReadSiteResponseTruncatedBody(t *testing.T) {
	body := "{\"urls\":[],\"size\":4}\nab"
	result, err := readSiteResponse(testSiteResponse(http.StatusOK, body, -1))
	if err == nil || result.complete {
		t.Fatalf("truncated body returned complete=%t err=%v", result.complete, err)
	}
}

func TestReadSiteResponseIncorrectContentLength(t *testing.T) {
	body := "{\"urls\":[],\"size\":4}\nabcd"
	result, err := readSiteResponse(testSiteResponse(http.StatusOK, body, int64(len(body)+1)))
	if err == nil || result.complete {
		t.Fatalf("incorrect Content-Length returned complete=%t err=%v", result.complete, err)
	}
}

func TestReadSiteResponseReadError(t *testing.T) {
	header := []byte("{\"urls\":[],\"size\":4}\n")
	body := &scriptedReadCloser{
		chunks:   [][]byte{header, []byte("ab")},
		finalErr: errors.New("injected body read failure"),
	}
	response := &http.Response{StatusCode: http.StatusOK, Body: body, ContentLength: -1}
	result, err := readSiteResponse(response)
	if err == nil || result.complete {
		t.Fatalf("read error returned complete=%t err=%v", result.complete, err)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestReadSiteResponseNon200(t *testing.T) {
	body := "not found\n"
	result, err := readSiteResponse(testSiteResponse(http.StatusNotFound, body, int64(len(body))))
	if err != nil {
		t.Fatalf("non-200 drain: %v", err)
	}
	if result.complete {
		t.Fatal("non-200 response marked complete")
	}
	if result.receivedBytes != int64(len(body)) {
		t.Fatalf("received bytes = %d, want %d", result.receivedBytes, len(body))
	}
}

func TestReadSiteResponseCancellation(t *testing.T) {
	header := []byte("{\"urls\":[],\"size\":4}\n")
	body := &scriptedReadCloser{
		chunks:   [][]byte{header},
		finalErr: context.Canceled,
	}
	result, err := readSiteResponse(&http.Response{
		StatusCode:    http.StatusOK,
		Body:          body,
		ContentLength: -1,
	})
	if err == nil || !errors.Is(err, context.Canceled) || result.complete {
		t.Fatalf("cancellation returned complete=%t err=%v", result.complete, err)
	}
}

func TestFetchRecordsCompletenessAndNon200Status(t *testing.T) {
	tests := []struct {
		name         string
		response     *http.Response
		transportErr error
		wantStatus   int
	}{
		{
			name:       "complete",
			response:   testSiteResponse(http.StatusOK, "{\"urls\":[],\"size\":4}\nabcd", -1),
			wantStatus: http.StatusOK,
		},
		{
			name:       "truncated",
			response:   testSiteResponse(http.StatusOK, "{\"urls\":[],\"size\":4}\nab", -1),
			wantStatus: 0,
		},
		{
			name:       "non-200",
			response:   testSiteResponse(http.StatusNotFound, "not found\n", -1),
			wantStatus: http.StatusNotFound,
		},
		{
			name:         "cancellation",
			transportErr: context.Canceled,
			wantStatus:   0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			driver := &ClientDriver{
				siteAddr: "fake-site.invalid",
				out:      bufio.NewWriter(io.Discard),
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return test.response, test.transportErr
			})}
			driver.fetch(context.Background(), client, "client", "/", 0)
			rows := driver.resultRows()
			if len(rows) != 1 || rows[0].status != test.wantStatus {
				t.Fatalf("rows = %+v, want one status %d", rows, test.wantStatus)
			}
		})
	}
}

func TestClientDriverCsvIdentityMatchesEmittedRows(t *testing.T) {
	var output bytes.Buffer
	driver := &ClientDriver{out: bufio.NewWriter(&output)}
	driver.writeCsvHeader()
	driver.writeCsvRow(time.UnixMilli(1234), "client", "/", 0, 200, 12, 10*time.Millisecond, 20*time.Millisecond)
	if err := driver.flush(); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(output.Bytes())
	gotHash, gotBytes := driver.csvIdentity()
	if gotHash != fmt.Sprintf("%x", want) || gotBytes != int64(output.Len()) {
		t.Fatalf("CSV identity = (%s, %d), want (%x, %d)", gotHash, gotBytes, want, output.Len())
	}
}
