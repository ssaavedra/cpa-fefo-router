package router

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// PluginName is also the expected library file name and config key.
	PluginName = "fefo-router"
	repository = "https://github.com/ssaavedra/cpa-fefo-router"

	pathStatus   = "/fefo/status"
	pathMetrics  = "/fefo/metrics"
	pathOverride = "/fefo/override"
	pathConfig   = "/fefo/config"

	pageCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'self'; base-uri 'none'; form-action 'none'"
)

//go:embed ui.html
var uiPage []byte

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// capabilities uses the host's JSON keys (internal/pluginhost/rpc_schema.go).
type capabilities struct {
	Scheduler                        bool `json:"scheduler"`
	RequestInterceptor               bool `json:"request_interceptor"`
	RequestInterceptorOmitBodyBefore bool `json:"request_interceptor_omit_body_before,omitempty"`
	UsagePlugin                      bool `json:"usage_plugin"`
	ManagementAPI                    bool `json:"management_api"`
}

type managementRegistration struct {
	Routes    []pluginapi.ManagementRoute `json:"routes"`
	Resources []resourceRoute             `json:"resources"`
}

// resourceRoute is a page CPA serves under /v0/resource/plugins/<id>/<path>
// and lists in the management panel's plugin menu. Resource requests are not
// authenticated, so the page is static; it calls the authenticated routes.
type resourceRoute struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

// Handler dispatches host RPC methods to the router.
type Handler struct {
	Router       *Router
	OnConfigured func()
}

// Handle returns the JSON envelope for one host call and never panics.
// Scheduler and interceptor failures degrade to "no decision" because the
// host fails the client request on plugin errors.
func (h *Handler) Handle(method string, request []byte) (out []byte) {
	defer func() {
		if p := recover(); p != nil {
			out = h.failure(method, fmt.Errorf("internal error: %v", p))
		}
	}()
	result, err := h.dispatch(method, request)
	if err != nil {
		return h.failure(method, err)
	}
	return result
}

func (h *Handler) failure(method string, err error) []byte {
	if h.Router != nil && h.Router.log != nil {
		h.Router.log("warn", "fefo-router call failed", map[string]any{"method": method, "error": err.Error()})
	}
	switch method {
	case pluginabi.MethodSchedulerPick:
		raw, _ := okEnvelope(pluginapi.SchedulerPickResponse{})
		return raw
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		raw, _ := okEnvelope(pluginapi.RequestInterceptResponse{})
		return raw
	}
	raw, _ := pluginabi.NewErrorEnvelope("plugin_error", err.Error())
	return raw
}

func (h *Handler) dispatch(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if err := json.Unmarshal(request, &req); err != nil {
				return nil, fmt.Errorf("decode lifecycle request: %w", err)
			}
		}
		cfg, err := ParseConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		h.Router.SetBaseConfig(req.ConfigYAML, cfg)
		if h.OnConfigured != nil {
			h.OnConfigured()
		}
		return okEnvelope(h.registration(cfg))
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		h.Router.StopRefresher()
		return okEnvelope(struct{}{})
	case pluginabi.MethodSchedulerPick:
		var req pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode scheduler pick: %w", err)
		}
		return okEnvelope(h.Router.Pick(req))
	case pluginabi.MethodRequestInterceptBefore:
		var req pluginapi.RequestInterceptRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode intercept: %w", err)
		}
		return okEnvelope(h.Router.Intercept(req))
	case pluginabi.MethodRequestInterceptAfter:
		// The capability covers both hooks; nothing to do after selection.
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	case pluginabi.MethodUsageHandle:
		var rec pluginapi.UsageRecord
		if err := json.Unmarshal(request, &rec); err != nil {
			return nil, fmt.Errorf("decode usage record: %w", err)
		}
		h.Router.ObserveUsage(rec)
		return okEnvelope(struct{}{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration{
			Routes: []pluginapi.ManagementRoute{
				{Method: http.MethodGet, Path: pathStatus, Description: "FEFO router status: accounts, spare, client plans, traffic, recent decisions. ?format=text for a table."},
				{Method: http.MethodGet, Path: pathMetrics, Description: "Prometheus metrics."},
				{Method: http.MethodPost, Path: pathOverride, Description: `Temporary override: {"client":"c","policy":"any","for":"2h"}, {"spare_factor":2,"for":"1h"} or {"client":"c","clear":true}.`},
				{Method: http.MethodGet, Path: pathConfig, Description: "Effective routing config, config.yaml section and runtime config."},
				{Method: http.MethodPost, Path: pathConfig, Description: `Set runtime config: {"config":{...}}, {"yaml":"..."}, {"assign_key":{"scope":"…","client":"c"}} or {"revert":true}.`},
			},
			Resources: []resourceRoute{{Path: "ui", Menu: "FEFO Router", Description: "Where requests go and why; overrides and routing config."}},
		})
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("decode management request: %w", err)
		}
		return okEnvelope(h.management(req))
	default:
		raw, _ := pluginabi.NewErrorEnvelope("unknown_method", "unknown method: "+method)
		return raw, nil
	}
}

func (h *Handler) registration(cfg Config) registration {
	f := func(name string, typ pluginapi.ConfigFieldType, desc string, enum ...string) pluginapi.ConfigField {
		return pluginapi.ConfigField{Name: name, Type: typ, Description: desc, EnumValues: enum}
	}
	active := cfg.Mode == ModeActive
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             PluginName,
			Version:          Version,
			Author:           "ssaavedra",
			GitHubRepository: repository,
			ConfigFields: []pluginapi.ConfigField{
				f("mode", pluginapi.ConfigFieldTypeEnum, "active routes traffic; observe only records usage (restart CPA after changing).", string(ModeActive), string(ModeObserve)),
				f("spare_factor", pluginapi.ConfigFieldTypeNumber, "Lend an account while remaining > factor × expected home usage until its reset. Default 1.5."),
				f("min_history", pluginapi.ConfigFieldTypeString, "History needed before demand is trusted and lending starts. Default 6h."),
				f("min_spare", pluginapi.ConfigFieldTypeNumber, "Spare needed to start lending, as a share of the account's weekly capacity. Default 0.03."),
				f("capacity", pluginapi.ConfigFieldTypeObject, "Relative plan size per account (Pro = 1): the capacity prior until dollars are learned."),
				f("capacity_usd", pluginapi.ConfigFieldTypeObject, "Pin an account's weekly capacity in API-equivalent USD instead of learning it."),
				f("default_unit_usd", pluginapi.ConfigFieldTypeNumber, "USD per unit of capacity before any account of the provider is learned. Default 220."),
				f("learn_min_delta", pluginapi.ConfigFieldTypeNumber, "Weekly share a cycle must see before its capacity estimate is used. Default 0.05."),
				f("state_file", pluginapi.ConfigFieldTypeString, "Where to persist quota and usage history."),
				f("log_decisions", pluginapi.ConfigFieldTypeBoolean, "Log every routing decision with the chosen account and why."),
			},
		},
		Capabilities: capabilities{
			Scheduler:                        active,
			RequestInterceptor:               active,
			RequestInterceptorOmitBodyBefore: active,
			UsagePlugin:                      true,
			ManagementAPI:                    true,
		},
	}
}

func (h *Handler) management(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	jsonH := http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}
	textH := http.Header{"Content-Type": {"text/plain; charset=utf-8"}, "Cache-Control": {"no-store"}}
	path := strings.TrimRight(req.Path, "/")
	method := strings.ToUpper(req.Method)
	errJSON := func(code int, msg string) pluginapi.ManagementResponse {
		body, _ := json.Marshal(map[string]string{"error": msg})
		return pluginapi.ManagementResponse{StatusCode: code, Headers: jsonH, Body: body}
	}
	switch {
	case strings.HasPrefix(path, "/v0/resource/plugins/"):
		return pluginapi.ManagementResponse{StatusCode: 200, Headers: http.Header{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"Content-Security-Policy": {pageCSP},
			"Referrer-Policy":         {"no-referrer"},
			"X-Content-Type-Options":  {"nosniff"},
		}, Body: uiPage}
	case strings.HasSuffix(path, pathConfig):
		if method == http.MethodPost {
			v, err := h.Router.UpdateConfig(req.Body)
			if err != nil {
				return errJSON(400, err.Error())
			}
			body, _ := json.Marshal(v)
			return pluginapi.ManagementResponse{StatusCode: 200, Headers: jsonH, Body: body}
		}
		body, _ := json.Marshal(h.Router.ConfigView())
		return pluginapi.ManagementResponse{StatusCode: 200, Headers: jsonH, Body: body}
	case strings.HasSuffix(path, pathStatus):
		n := 30
		if v, err := strconv.Atoi(req.Query.Get("decisions")); err == nil && v >= 0 {
			n = v
		}
		st := h.Router.Status(n)
		if req.Query.Get("format") == "text" {
			return pluginapi.ManagementResponse{StatusCode: 200, Headers: textH, Body: []byte(st.Text())}
		}
		body, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return errJSON(500, "encode status: "+err.Error())
		}
		return pluginapi.ManagementResponse{StatusCode: 200, Headers: jsonH, Body: body}
	case strings.HasSuffix(path, pathMetrics):
		return pluginapi.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/plain; version=0.0.4"}}, Body: []byte(h.Router.Prometheus())}
	case strings.HasSuffix(path, pathOverride):
		if method != http.MethodPost {
			return errJSON(405, "use POST")
		}
		o, err := h.Router.ApplyOverride(req.Body)
		if err != nil {
			return errJSON(400, err.Error())
		}
		body, _ := json.Marshal(map[string]any{"ok": true, "override": o})
		return pluginapi.ManagementResponse{StatusCode: 200, Headers: jsonH, Body: body}
	}
	return errJSON(404, "not found")
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}
