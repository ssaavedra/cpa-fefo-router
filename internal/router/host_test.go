package router

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func envelope(t *testing.T, result any) []byte {
	t.Helper()
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(pluginabi.Envelope{OK: true, Result: body})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHostClientAuthJSON(t *testing.T) {
	var gotMethod string
	var gotPayload []byte
	h := HostClient{Call: func(method string, payload []byte) ([]byte, int) {
		gotMethod, gotPayload = method, payload
		return envelope(t, pluginapi.HostAuthGetResponse{AuthIndex: "7", JSON: json.RawMessage(`{"access_token":"tok"}`)}), 0
	}}
	got, err := h.AuthJSON(context.Background(), "7")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != pluginabi.MethodHostAuthGet || string(gotPayload) != `{"auth_index":"7"}` {
		t.Fatalf("call = %s %s", gotMethod, gotPayload)
	}
	if string(got) != `{"access_token":"tok"}` {
		t.Fatalf("json = %s", got)
	}
}

func TestHostClientHTTPDoRoundTripsWireFormat(t *testing.T) {
	var gotMethod string
	var sent map[string]any
	h := HostClient{Call: func(method string, payload []byte) ([]byte, int) {
		gotMethod = method
		if err := json.Unmarshal(payload, &sent); err != nil {
			t.Fatal(err)
		}
		return envelope(t, pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"ok":1}`)}), 0
	}}
	resp, err := h.HTTPDo(context.Background(), pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     "https://api.anthropic.com/api/oauth/usage",
		Headers: http.Header{"Anthropic-Beta": {"oauth-2025-04-20"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != pluginabi.MethodHostHTTPDo {
		t.Fatalf("method = %s", gotMethod)
	}
	// The host decodes these keys case-insensitively into its own request type.
	lower := map[string]any{}
	for k, v := range sent {
		lower[strings.ToLower(k)] = v
	}
	if lower["method"] != "GET" || lower["url"] != "https://api.anthropic.com/api/oauth/usage" || lower["headers"] == nil {
		t.Fatalf("payload = %v", sent)
	}
	if resp.StatusCode != 200 || string(resp.Body) != `{"ok":1}` {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestHostClientCallbackErrors(t *testing.T) {
	failing := HostClient{Call: func(string, []byte) ([]byte, int) {
		out, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: &pluginabi.Error{Code: "not_found", Message: "no such auth"}})
		return out, 1
	}}
	if _, err := failing.AuthJSON(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "no such auth") {
		t.Fatalf("err = %v", err)
	}
	if _, err := (HostClient{}).HTTPDo(context.Background(), pluginapi.HTTPRequest{}); err == nil {
		t.Fatal("expected error without host callbacks")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := failing.HTTPDo(ctx, pluginapi.HTTPRequest{}); err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
}

func TestClaudeAccessToken(t *testing.T) {
	tok, err := ClaudeAccessToken([]byte(`{"type":"claude","access_token":"secret-value","refresh_token":"r"}`))
	if err != nil || tok != "secret-value" {
		t.Fatalf("tok, err = %q, %v", tok, err)
	}
	for _, raw := range []string{`{"type":"claude"}`, `not json secret-value`} {
		_, err := ClaudeAccessToken([]byte(raw))
		if err == nil || strings.Contains(err.Error(), "secret-value") {
			t.Fatalf("err for %q = %v", raw, err)
		}
	}
}
