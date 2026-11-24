package pricing

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// semantics says how a provider reports token buckets.
type semantics struct {
	inputIncludesCache bool
	reasoningInOutput  bool
}

// Mirrors tokenAccountingSemanticsFor in CPA's sdk/cliproxy/usage/accounting.go.
// The first matching marker wins; unknown providers are treated like OpenAI.
var providerSemantics = []struct {
	markers []string
	semantics
}{
	{[]string{"claude", "anthropic"}, semantics{inputIncludesCache: false, reasoningInOutput: false}},
	{[]string{"gemini", "aistudio", "antigravity", "vertex", "interaction"}, semantics{inputIncludesCache: true, reasoningInOutput: false}},
}

var subsetSemantics = semantics{inputIncludesCache: true, reasoningInOutput: true}

func semanticsFor(provider string) semantics {
	p := strings.ToLower(provider)
	for _, e := range providerSemantics {
		for _, m := range e.markers {
			if strings.Contains(p, m) {
				return e.semantics
			}
		}
	}
	return subsetSemantics
}

// NormalizeUsage converts a provider's usage report into billing buckets.
func NormalizeUsage(provider string, d pluginapi.UsageDetail) Usage {
	s := semanticsFor(provider)
	write := max(d.CacheCreationTokens, 0)
	read := max(d.CacheReadTokens, 0)
	if read == 0 {
		// CachedTokens may or may not include cache writes; only the remainder is a read.
		read = max(d.CachedTokens-write, 0)
	}
	u := Usage{Uncached: max(d.InputTokens, 0), CacheRead: read, CacheWrite: write, Output: max(d.OutputTokens, 0)}
	if s.inputIncludesCache {
		u.Uncached = max(u.Uncached-read-write, 0)
	}
	if !s.reasoningInOutput {
		u.Output += max(d.ReasoningTokens, 0)
	}
	return u
}
