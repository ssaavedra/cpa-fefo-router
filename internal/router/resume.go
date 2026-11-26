package router

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// DefaultResumePrompts holds what T3 Code sends to carry on a thread that a
// usage limit stopped.
var DefaultResumePrompts = []string{"Continue where you left off."}

// PatchBody edits a request body before credential selection.
func (r *Router) PatchBody(body []byte) []pluginapi.RequestBodyEdit {
	cfg := r.Config()
	edits := dropResumePrompts(body, cfg.ResumePrompts)
	if len(edits) > 0 && cfg.LogDecisions && r.log != nil {
		r.log("info", "fefo dropped resume prompts merged after a usage limit", map[string]any{"messages": len(edits)})
	}
	return edits
}

// dropResumePrompts returns edits that take resume prompts out of the user
// messages they were merged into.
//
// A request refused for a usage limit leaves no reply. The Claude CLI keeps
// its error message out of later requests and merges every prompt sent after
// it into the user message the limit interrupted. After two failed resumes the
// model would read that message followed by three copies of the resume prompt.
// Without the copies the interrupted request goes out as it was, and the model
// carries on with it. A user message holding nothing but resume prompts keeps
// its last one, and so does one sent as a plain string.
func dropResumePrompts(body []byte, prompts []string) []pluginapi.RequestBodyEdit {
	if !slices.ContainsFunc(prompts, func(p string) bool { return p != "" && bytes.Contains(body, []byte(p)) }) {
		return nil
	}
	messages, ok := objectField(body, "messages")
	if !ok {
		return nil
	}
	list, ok := arrayElems(messages.raw)
	if !ok {
		return nil
	}
	var edits []pluginapi.RequestBodyEdit
	for _, m := range list {
		var head struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(m.raw, &head) != nil || head.Role != "user" {
			continue
		}
		content, ok := objectField(m.raw, "content")
		if !ok {
			continue
		}
		blocks, ok := arrayElems(content.raw)
		if !ok {
			continue
		}
		replacement, ok := withoutResumePrompts(blocks, prompts)
		if !ok {
			continue
		}
		start := messages.start + m.start + content.start
		edits = append(edits, pluginapi.RequestBodyEdit{Start: start, End: start + len(content.raw), Replacement: replacement})
	}
	return edits
}

// withoutResumePrompts rebuilds a content array without its resume prompts.
// The other blocks keep their bytes. A cache breakpoint on a dropped block
// moves to the last block kept, so the request caches as far as before.
func withoutResumePrompts(blocks []span, prompts []string) (string, bool) {
	type block struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	parsed := make([]block, len(blocks))
	isPrompt := make([]bool, len(blocks))
	n := 0
	for i, b := range blocks {
		if json.Unmarshal(b.raw, &parsed[i]) == nil && parsed[i].Type == "text" && slices.Contains(prompts, strings.TrimSpace(parsed[i].Text)) {
			isPrompt[i] = true
			n++
		}
	}
	if n == 0 || n == 1 && len(blocks) == 1 {
		return "", false
	}
	keep := -1 // a message of resume prompts only keeps its last
	if n == len(blocks) {
		keep = len(blocks) - 1
	}
	var kept [][]byte
	var cacheControl json.RawMessage
	lastKept := -1
	for i, b := range blocks {
		if isPrompt[i] && i != keep {
			if len(parsed[i].CacheControl) > 0 {
				cacheControl = parsed[i].CacheControl
			}
			continue
		}
		kept = append(kept, b.raw)
		lastKept = i
	}
	if cacheControl != nil && len(parsed[lastKept].CacheControl) == 0 {
		kept[len(kept)-1] = withField(kept[len(kept)-1], "cache_control", cacheControl)
	}
	return "[" + string(bytes.Join(kept, []byte(","))) + "]", true
}

// withField adds a field to a JSON object; anything else comes back as is.
func withField(obj json.RawMessage, key string, value json.RawMessage) json.RawMessage {
	if len(obj) < 2 || obj[0] != '{' || obj[len(obj)-1] != '}' {
		return obj
	}
	name, _ := json.Marshal(key)
	field := append(append(name, ':'), value...)
	inner := bytes.TrimSpace(obj[1 : len(obj)-1])
	out := append([]byte{'{'}, inner...)
	if len(inner) > 0 {
		out = append(out, ',')
	}
	return append(append(out, field...), '}')
}

// span is a JSON value and its offset in the document it was read from.
type span struct {
	start int
	raw   json.RawMessage
}

// objectField finds a field of a JSON object.
func objectField(obj []byte, key string) (span, bool) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return span{}, false
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return span{}, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return span{}, false
		}
		if k, _ := t.(string); k == key {
			return located(obj, v, dec.InputOffset())
		}
	}
	return span{}, false
}

// arrayElems lists the elements of a JSON array; false if it is not one.
func arrayElems(arr []byte) ([]span, bool) {
	dec := json.NewDecoder(bytes.NewReader(arr))
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return nil, false
	}
	var out []span
	for dec.More() {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		s, ok := located(arr, v, dec.InputOffset())
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// located places a value the decoder just read, which ends at end.
func located(doc []byte, v json.RawMessage, end int64) (span, bool) {
	start := int(end) - len(v)
	if start < 0 || !bytes.Equal(doc[start:end], v) {
		return span{}, false
	}
	return span{start: start, raw: v}, true
}
