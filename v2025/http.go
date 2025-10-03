package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const DefaultHttpTimeout = 30 * time.Second
const DefaultHttpConnectTimeout = 5 * time.Second
const DefaultHttpTlsTimeout = 5 * time.Second

func DefaultHttpClient() *http.Client {
	// see https://medium.com/@nate510/don-t-use-go-s-default-http-client-4804cb19f779
	dialer := &net.Dialer{
		Timeout: DefaultHttpConnectTimeout,
	}
	transport := &http.Transport{
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: DefaultHttpTlsTimeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   DefaultHttpTimeout,
	}
}

type HeaderCallback func(header http.Header)
type ResponseCallback[R any] func(response *http.Response, responseBodyBytes []byte) (R, error)

type Status struct {
	Code   int
	Status string
}

func NewStatusFromResponse(response *http.Response) *Status {
	return &Status{
		Code:   response.StatusCode,
		Status: response.Status,
	}
}

func HttpPostRequireStatusOk[R any](
	ctx context.Context,
	url string,
	requestBody any,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (R, error) {
	return HttpPost[R](
		ctx,
		url,
		requestBody,
		headerCallback,
		HttpResponseRequireStatusOk[R](responseCallback),
	)
}

func HttpPostRawRequireStatusOk(
	ctx context.Context,
	url string,
	requestBody []byte,
	headerCallback HeaderCallback,
) ([]byte, error) {
	return HttpPost[[]byte](
		ctx,
		url,
		requestBody,
		headerCallback,
		HttpResponseRequireStatusOk[[]byte](func(response *http.Response, responseBodyBytes []byte) ([]byte, error) {
			return responseBodyBytes, nil
		}),
	)
}

func HttpPostBasic[R any](
	ctx context.Context,
	url string,
	requestBody any,
) (R, error) {
	return HttpPost(ctx, url, requestBody, NoCustomHeaders, ResponseJsonObject[R])
}

func HttpPostBasicWithStatus[R any](
	ctx context.Context,
	url string,
	requestBody any,
) (*Status, R, error) {
	return HttpPostWithStatus(ctx, url, requestBody, NoCustomHeaders, ResponseJsonObject[R])
}

func HttpPost[R any](
	ctx context.Context,
	url string,
	requestBody any,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (r R, err error) {
	_, r, err = HttpPostWithStatus(ctx, url, requestBody, headerCallback, responseCallback)
	return
}

func HttpPostWithStatus[R any](
	ctx context.Context,
	url string,
	requestBody any,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (*Status, R, error) {
	var empty R

	requestBodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return nil, empty, err
	}

	request, err := http.NewRequestWithContext(
		ctx,
		"POST",
		url,
		bytes.NewReader(requestBodyBytes),
	)
	if err != nil {
		return nil, empty, err
	}

	header := request.Header
	header.Add("Content-Type", "application/json")
	headerCallback(header)

	client := DefaultHttpClient()

	response, err := client.Do(request)
	if err != nil {
		return nil, empty, err
	}
	defer response.Body.Close()

	responseBodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, empty, err
	}

	r, err := responseCallback(response, responseBodyBytes)
	return NewStatusFromResponse(response), r, err
}

func HttpPostForm[R any](
	ctx context.Context,
	url string,
	form url.Values,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (R, error) {
	var empty R

	request, err := http.NewRequestWithContext(
		ctx,
		"POST",
		url,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return empty, err
	}

	header := request.Header
	header.Add("Content-Type", "application/x-www-form-urlencoded")
	headerCallback(header)

	client := DefaultHttpClient()

	response, err := client.Do(request)
	if err != nil {
		return empty, err
	}
	defer response.Body.Close()

	responseBodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return empty, err
	}

	return responseCallback(response, responseBodyBytes)
}

func HttpGetRequireStatusOk[R any](
	ctx context.Context,
	url string,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (R, error) {
	return HttpGet[R](
		ctx,
		url,
		headerCallback,
		HttpResponseRequireStatusOk[R](responseCallback),
	)
}

func HttpGetRawRequireStatusOk(
	ctx context.Context,
	url string,
	headerCallback HeaderCallback,
) ([]byte, error) {
	return HttpGet[[]byte](
		ctx,
		url,
		headerCallback,
		HttpResponseRequireStatusOk[[]byte](func(response *http.Response, responseBodyBytes []byte) ([]byte, error) {
			return responseBodyBytes, nil
		}),
	)
}

func HttpGetBasic[R any](
	ctx context.Context,
	url string,
) (R, error) {
	return HttpGet(ctx, url, NoCustomHeaders, ResponseJsonObject[R])
}

func HttpGetBasicWithStatus[R any](
	ctx context.Context,
	url string,
) (*Status, R, error) {
	return HttpGetWithStatus(ctx, url, NoCustomHeaders, ResponseJsonObject[R])
}

func HttpGet[R any](
	ctx context.Context,
	url string,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (r R, err error) {
	_, r, err = HttpGetWithStatus(ctx, url, headerCallback, responseCallback)
	return
}

func HttpGetWithStatus[R any](
	ctx context.Context,
	url string,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (*Status, R, error) {
	var empty R

	request, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, empty, err
	}

	header := request.Header
	headerCallback(header)

	client := DefaultHttpClient()

	response, err := client.Do(request)
	if err != nil {
		return nil, empty, err
	}
	defer response.Body.Close()

	responseBodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, empty, err
	}

	r, err := responseCallback(response, responseBodyBytes)
	return NewStatusFromResponse(response), r, err
}

func HttpDeleteBasic[R any](
	ctx context.Context,
	url string,
) (R, error) {
	return HttpDelete(ctx, url, NoCustomHeaders, ResponseJsonObject[R])
}

func HttpDeleteBasicWithStatus[R any](
	ctx context.Context,
	url string,
) (*Status, R, error) {
	return HttpDeleteWithStatus(ctx, url, NoCustomHeaders, ResponseJsonObject[R])
}

func HttpDelete[R any](
	ctx context.Context,
	url string,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (r R, err error) {
	_, r, err = HttpDeleteWithStatus(ctx, url, headerCallback, responseCallback)
	return
}

func HttpDeleteWithStatus[R any](
	ctx context.Context,
	url string,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (*Status, R, error) {
	var empty R

	request, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return nil, empty, err
	}

	header := request.Header
	headerCallback(header)

	client := DefaultHttpClient()

	response, err := client.Do(request)
	if err != nil {
		return nil, empty, err
	}
	defer response.Body.Close()

	responseBodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, empty, err
	}

	r, err := responseCallback(response, responseBodyBytes)
	return NewStatusFromResponse(response), r, err
}

func NoCustomHeaders(header http.Header) {
	// no nothing
}

func ResponseJsonObject[R any](response *http.Response, responseBodyBytes []byte) (R, error) {
	var result R
	err := json.Unmarshal(responseBodyBytes, &result)
	if err != nil {
		return result, err
	}
	return result, nil
}

func HttpResponseRequireStatusOk[R any](responseCallback ResponseCallback[R]) ResponseCallback[R] {
	return func(response *http.Response, responseBodyBytes []byte) (R, error) {
		// 2xx
		if 200 <= response.StatusCode && response.StatusCode < 300 {
			return responseCallback(response, responseBodyBytes)
		}
		err := &HttpStatusError{
			StatusCode:   response.StatusCode,
			Status:       response.Status,
			ResponseBody: string(responseBodyBytes),
		}
		var empty R
		return empty, err
	}
}

type HttpStatusError struct {
	StatusCode   int
	Status       string
	ResponseBody string
}

func (self *HttpStatusError) Error() string {
	return fmt.Sprintf("Bad status: %s %s", self.Status, self.ResponseBody)
}

func ListenAndServeWithReusePort(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	listenConfig := net.ListenConfig{
		Control: func(network string, address string, rawConn syscall.RawConn) error {
			var setErr error
			err := rawConn.Control(func(fd uintptr) {
				setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			})
			return errors.Join(err, setErr)
		},
	}

	listener, err := listenConfig.Listen(ctx, "tcp", server.Addr)
	if err != nil {
		return err
	}

	return server.Serve(listener)
}
