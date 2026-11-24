package router

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const modelsDevURL = "https://models.dev/api.json"

// fakeHost stands in for host.auth.get and host.http.do.
type fakeHost struct {
	mu       sync.Mutex
	creds    map[string]string // auth index -> credential JSON
	routes   map[string]func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	requests []pluginapi.HTTPRequest
}

func newFakeHost() *fakeHost {
	return &fakeHost{creds: map[string]string{}, routes: map[string]func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error){}}
}

func (f *fakeHost) respond(url string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[url] = func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)}, nil
	}
}

func (f *fakeHost) AuthJSON(_ context.Context, authIndex string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.creds[authIndex]
	if !ok {
		return nil, errors.New("host.auth.get: not_found: no auth with index " + authIndex)
	}
	return json.RawMessage(c), nil
}

func (f *fakeHost) HTTPDo(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	route := f.routes[req.URL]
	f.mu.Unlock()
	if route == nil {
		return pluginapi.HTTPResponse{StatusCode: 404}, nil
	}
	return route(req)
}

func (f *fakeHost) sent(url string) []pluginapi.HTTPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []pluginapi.HTTPRequest
	for _, r := range f.requests {
		if r.URL == url {
			out = append(out, r)
		}
	}
	return out
}

func (h *harness) refresh() {
	h.t.Helper()
	if err := h.r.Refresh(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// metric returns the value of one Prometheus sample, "" when absent.
func (h *harness) metric(series string) string {
	for _, line := range strings.Split(h.r.Prometheus(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			return v
		}
	}
	return ""
}

func requireMetric(t *testing.T, h *harness, series, want string) {
	t.Helper()
	if got := h.metric(series); got != want {
		t.Fatalf("%s = %q, want %q", series, got, want)
	}
}
