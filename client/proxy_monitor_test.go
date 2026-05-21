package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"testing"
)

func TestShouldMonitorAIEndpointSkipsTelemetry(t *testing.T) {
	cases := []struct {
		endpoint string
		want     bool
	}{
		{endpoint: "/ces/v1/telemetry/intake", want: false},
		{endpoint: "/v1/telemetry/intake", want: false},
		{endpoint: "/v1/models", want: false},
		{endpoint: "/api/v1/models", want: false},
		{endpoint: "/models/session", want: false},
		{endpoint: "/agents", want: false},
		{endpoint: "/agents/sessions", want: false},
		{endpoint: "/agents/sessions/session-123", want: false},
		{endpoint: "/agents/sessions/session-123/messages", want: false},
		{endpoint: "/agents/swe/models", want: false},
		{endpoint: "/agents/swe/v1/jobs/chernzhi/Tokens-Monitor/enabled", want: false},
		{endpoint: "/extensions-control", want: false},
		{endpoint: "/tev1/v1/rgstr", want: false},
		{endpoint: "/backend-api/accounts/check", want: false},
		{endpoint: "/backend-api/codex/responses/compact", want: false},
		{endpoint: "/backend-api/wham/usage", want: false},
		{endpoint: "/backend-api/codex/responses", want: true},
		{endpoint: "/v1/chat/completions", want: true},
	}

	for _, tc := range cases {
		if got := shouldMonitorAIEndpoint(tc.endpoint); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.endpoint, got, tc.want)
		}
	}
}

func TestPeekAndRestoreResponseBodyDoesNotTruncate(t *testing.T) {
	body := []byte("abcdefghijklmnopqrstuvwxyz")
	resp := &http.Response{Body: io.NopCloser(bytes.NewReader(body))}

	peek := peekAndRestoreResponseBody(resp, 5)
	if string(peek) != "abcde" {
		t.Fatalf("peek = %q, want %q", string(peek), "abcde")
	}

	restored, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if !bytes.Equal(restored, body) {
		t.Fatalf("restored body = %q, want %q", string(restored), string(body))
	}
}

func TestExpectedDisconnectErrors(t *testing.T) {
	cases := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		errors.New("read tcp 127.0.0.1:18090->127.0.0.1:57099: wsarecv: An existing connection was forcibly closed by the remote host."),
		errors.New("write tcp 127.0.0.1:18090->127.0.0.1:57099: broken pipe"),
		errors.New("read tcp: connection reset by peer"),
	}

	for _, err := range cases {
		if !isExpectedDisconnectError(err) {
			t.Fatalf("expected disconnect not recognized: %v", err)
		}
	}
}

func TestLinkLocalMetadataHost(t *testing.T) {
	if !isLinkLocalMetadataHost("169.254.169.254") {
		t.Fatal("metadata host should be link-local metadata")
	}
	if isLinkLocalMetadataHost("api.openai.com") {
		t.Fatal("api.openai.com should not be metadata host")
	}
}
