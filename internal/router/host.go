package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// HostCall performs one raw host callback through the C ABI and returns the
// response envelope and the C return code.
type HostCall func(method string, payload []byte) ([]byte, int)

// HostClient adapts HostCall to the router's AuthLister and LogFunc.
type HostClient struct {
	Call HostCall
}

type hostAuthList struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostLogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// ListAuths calls host.auth.list. The C call cannot be cancelled, so ctx is
// only checked before the call.
func (h HostClient) ListAuths(ctx context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := h.invoke(pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return nil, err
	}
	var out hostAuthList
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", pluginabi.MethodHostAuthList, err)
	}
	return out.Files, nil
}

// AuthJSON calls host.auth.get and returns the credential JSON. The result
// holds secrets: callers must not log it.
func (h HostClient) AuthJSON(ctx context.Context, authIndex string) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := h.invoke(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return nil, err
	}
	var out pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", pluginabi.MethodHostAuthGet, err)
	}
	return out.JSON, nil
}

// HTTPDo calls host.http.do, which sends the request through CPA's proxy
// settings. Only ctx's state before the call is honoured.
func (h HostClient) HTTPDo(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if err := ctx.Err(); err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	raw, err := h.invoke(pluginabi.MethodHostHTTPDo, req)
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var out pluginapi.HTTPResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode %s: %w", pluginabi.MethodHostHTTPDo, err)
	}
	return out, nil
}

// ClaudeAccessToken reads the OAuth access token from Claude credential JSON.
// Errors never include credential content.
func ClaudeAccessToken(credJSON []byte) (string, error) {
	var cred struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(credJSON, &cred); err != nil {
		return "", errors.New("claude credential is not valid JSON")
	}
	if cred.AccessToken == "" {
		return "", errors.New("claude credential has no access_token")
	}
	return cred.AccessToken, nil
}

// Log forwards a diagnostic line to host.log. Failures are dropped: logging
// must never affect routing.
func (h HostClient) Log(level, msg string, fields map[string]any) {
	_, _ = h.invoke(pluginabi.MethodHostLog, hostLogRequest{Level: level, Message: msg, Fields: fields})
}

func (h HostClient) invoke(method string, payload any) (json.RawMessage, error) {
	if h.Call == nil {
		return nil, fmt.Errorf("%s: host callbacks unavailable", method)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	raw, code := h.Call(method, body)
	if len(raw) == 0 {
		return nil, fmt.Errorf("%s: empty host response (code %d)", method, code)
	}
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s: decode envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s: %s", method, env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("%s: host returned an error", method)
	}
	if code != 0 {
		return nil, fmt.Errorf("%s: host returned code %d", method, code)
	}
	return env.Result, nil
}
