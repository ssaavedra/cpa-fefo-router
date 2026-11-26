package router

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// applyEdits splices edits the way the host does.
func applyEdits(t *testing.T, body []byte, edits []pluginapi.RequestBodyEdit) []byte {
	t.Helper()
	var out []byte
	at := 0
	for _, e := range edits {
		if e.Start < at || e.End < e.Start || e.End > len(body) {
			t.Fatalf("bad edit %+v after %d", e, at)
		}
		out = append(append(out, body[at:e.Start]...), e.Replacement...)
		at = e.End
	}
	out = append(out, body[at:]...)
	if !json.Valid(out) {
		t.Fatalf("invalid JSON after edits: %s", out)
	}
	return out
}

// userContents lists each user message's content as JSON.
func userContents(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range req.Messages {
		if m.Role == "user" {
			out = append(out, string(m.Content))
		}
	}
	return out
}

// The shape the Claude CLI (2.1.332) sends after T3 Code resumed twice and
// the second resume got through: the interrupted prompt, then one resume
// prompt per attempt, merged into one user message.
const resumedBody = `{"model":"claude-opus-5-5","max_tokens":32000,
 "system":[{"type":"text","text":"You are Claude Code. Continue where you left off. is just text here"}],
 "messages":[
  {"role":"user","content":"Read the file"},
  {"role":"system","content":[{"type":"text","text":"hook context"}]},
  {"role":"assistant","content":[{"type":"text","text":"Continue where you left off."}]},
  {"role":"user","content":[{"type":"text","text":"Now fix the bug in it\n"},{"type":"text","text":"Continue where you left off.\n"},{"type":"text","text":"Continue where you left off.","cache_control":{"type":"ephemeral"}}]},
  {"role":"system","content":[{"type":"text","text":"hook context"}]}
 ],
 "stream":true}`

func TestDropResumePromptsCarriesOnWithTheInterruptedMessage(t *testing.T) {
	body := []byte(resumedBody)
	edits := dropResumePrompts(body, DefaultResumePrompts)
	if len(edits) != 1 {
		t.Fatalf("edits %+v", edits)
	}
	out := applyEdits(t, body, edits)
	got := userContents(t, out)
	want := []string{`"Read the file"`, `[{"type":"text","text":"Now fix the bug in it\n","cache_control":{"type":"ephemeral"}}]`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("user contents\n got %q\nwant %q", got, want)
	}
	// Everything outside the edited array keeps its bytes.
	if e := edits[0]; string(out[:e.Start]) != resumedBody[:e.Start] || !strings.HasSuffix(resumedBody, string(out[e.Start+len(e.Replacement):])) {
		t.Fatalf("bytes outside the edit changed:\n%s", out)
	}
}

func TestDropResumePromptsLeavesOtherMessagesAlone(t *testing.T) {
	for name, body := range map[string]string{
		"no resume prompt":   `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}]}`,
		"lone string prompt": `{"messages":[{"role":"user","content":"Continue where you left off."}]}`,
		"lone block prompt":  `{"messages":[{"role":"user","content":[{"type":"text","text":"Continue where you left off."}]}]}`,
		"assistant":          `{"messages":[{"role":"assistant","content":[{"type":"text","text":"x"},{"type":"text","text":"Continue where you left off."}]}]}`,
		"prompt inside text": `{"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"text","text":"Please Continue where you left off."}]}]}`,
		"not JSON":           `{"messages":[{"role":"user","content":[{"type":"text","text":"x"},{"type":"text","text":"Continue where you left off."}`,
		"no messages":        `{"input":"Continue where you left off."}`,
	} {
		if edits := dropResumePrompts([]byte(body), DefaultResumePrompts); len(edits) != 0 {
			t.Errorf("%s: %+v", name, edits)
		}
	}
	if edits := dropResumePrompts([]byte(resumedBody), nil); len(edits) != 0 {
		t.Errorf("turned off: %+v", edits)
	}
}

func TestDropResumePromptsKeepsOneWhenNothingElseIsLeft(t *testing.T) {
	// A tool result interrupted mid-turn, and a message of prompts only.
	body := []byte(`{"messages":[
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":"Continue where you left off."}]},
	  {"role":"assistant","content":[{"type":"text","text":"done"}]},
	  {"role":"user","content":[{"type":"text","text":"Continue where you left off.\n"},{"type":"text","text":"Continue where you left off."}]}]}`)
	edits := dropResumePrompts(body, DefaultResumePrompts)
	got := userContents(t, applyEdits(t, body, edits))
	want := []string{`[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`, `[{"type":"text","text":"Continue where you left off."}]`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("user contents\n got %q\nwant %q", got, want)
	}
}

func TestPatchBodyOverRPC(t *testing.T) {
	h := newHarness(t, "")
	hd := &Handler{Router: h.r}
	raw, err := hd.dispatch(pluginabi.MethodRequestPatchBodyBefore, []byte(resumedBody))
	if err != nil {
		t.Fatal(err)
	}
	var env pluginabi.Envelope
	var resp pluginapi.RequestBodyPatchResponse
	if json.Unmarshal(raw, &env) != nil || !env.OK || json.Unmarshal(env.Result, &resp) != nil || len(resp.Edits) != 1 {
		t.Fatalf("envelope %s", raw)
	}
	// Off when the list is emptied.
	h.r.SetConfig(testConfig(t, "resume_prompts: []\n"))
	if edits := h.r.PatchBody([]byte(resumedBody)); len(edits) != 0 {
		t.Fatalf("resume_prompts: [] still edits: %+v", edits)
	}
}
