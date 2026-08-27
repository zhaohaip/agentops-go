package eino

import (
	"bytes"
	"errors"
	"io"
	"net/http"
)

var errResponseTooLarge = errors.New("model response body exceeds limit")

type responseLimitTransport struct {
	next     http.RoundTripper
	maxBytes int64
}

func cloneHTTPClientWithResponseLimit(client *http.Client, maxBytes int64) *http.Client {
	if client == nil {
		client = &http.Client{}
	}
	clone := *client
	next := clone.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	clone.Transport = &responseLimitTransport{next: next, maxBytes: maxBytes}
	return &clone
}

func (t *responseLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.Body == nil {
		return response, nil
	}
	if response.ContentLength > t.maxBytes {
		_ = response.Body.Close()
		return nil, errResponseTooLarge
	}

	body, readErr := io.ReadAll(io.LimitReader(response.Body, t.maxBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > t.maxBytes {
		return nil, errResponseTooLarge
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	return response, nil
}
