package bringyour

import (
	"fmt"
	"time"
	"net"
	"net/http"
	"encoding/json"
	"bytes"
	"io"
)


const DefaultHttpTimeout = 10 * time.Second
const DefaultHttpConnectTimeout = 5 * time.Second
const DefaultHttpTlsTimeout = 5 * time.Second


func DefaultHttpClient() *http.Client {
	// see https://medium.com/@nate510/don-t-use-go-s-default-http-client-4804cb19f779
	dialer := &net.Dialer{
    	Timeout: DefaultHttpConnectTimeout,
  	}
	transport := &http.Transport{
	  	DialContext: dialer.DialContext,
	  	TLSHandshakeTimeout: DefaultHttpTlsTimeout,
	}
	return &http.Client{
		Transport: transport,
		Timeout: DefaultHttpTimeout,
	}
}


type HeaderCallback func(header http.Header)
type ResponseCallback[R any] func(response *http.Response, responseBodyBytes []byte)(R, error)


func HttpPostRequireStatusOk[R any](
	url string,
	requestBody any,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (R, error) {
	return HttpPost[R](
		url,
		requestBody,
		headerCallback,
		HttpResponseRequireStatusOk[R](responseCallback),
	)
}


func HttpPostBasic[R any](
	url string,
	requestBody any,
) (map[string]any, error) {
	return HttpPost(url, requestBody, NoCustomHeaders, ResponseJsonObject)
}


func HttpPost[R any](
	url string,
	requestBody any,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (R, error) {
	var empty R

    requestBodyBytes, err := json.Marshal(requestBody)
    if err != nil {
        return empty, err
    }

    fmt.Printf("POST BODY %s\n", string(requestBodyBytes))

    request, err := http.NewRequest(
        "POST",
        url,
        bytes.NewReader(requestBodyBytes),
    )
    if err != nil {
    	return empty, err
    }

    header := request.Header
    header.Add("Content-Type", "application/json")
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

    fmt.Printf("POST RESPONSE BODY %s\n", string(responseBodyBytes))

    return responseCallback(response, responseBodyBytes)
}


func HttpGetRequireStatusOk[R any](
	url string,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (R, error) {
	return HttpGet[R](
		url,
		headerCallback,
		HttpResponseRequireStatusOk[R](responseCallback),
	)
}


func HttpGetBasic[R any](
	url string,
) (map[string]any, error) {
	return HttpGet(url, NoCustomHeaders, ResponseJsonObject)
}


func HttpGet[R any](
	url string,
	headerCallback HeaderCallback,
	responseCallback ResponseCallback[R],
) (R, error) {
	var empty R

	fmt.Printf("GET %s\n", url)

    request, err := http.NewRequest("GET", url, nil)
    if err != nil {
    	return empty, err
    }

    header := request.Header
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

    fmt.Printf("GET RESPONSE BODY %s\n", string(responseBodyBytes))

    return responseCallback(response, responseBodyBytes)
}



func NoCustomHeaders(header http.Header) {
	// no nothing
}


func ResponseJsonObject(response *http.Response, responseBodyBytes []byte) (map[string]any, error) {
	obj := map[string]any{}
	err := json.Unmarshal(responseBodyBytes, &obj)
	if err != nil {
		return nil, err
	}
	return obj, nil
}


func HttpResponseRequireStatusOk[R any](responseCallback ResponseCallback[R]) ResponseCallback[R] {
	return func(response *http.Response, responseBodyBytes []byte)(R, error) {
		// 2xx
		if 200 <= response.StatusCode && response.StatusCode < 300 {
			return responseCallback(response, responseBodyBytes)
	    }
		var empty R
        return empty, fmt.Errorf("Bad status: %s %s", response.Status, string(responseBodyBytes))
	}
}