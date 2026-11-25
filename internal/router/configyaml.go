package router

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// spliceSection reads the plugin's section (plugins.configs.fefo-router) out
// of CPA's config.yaml and, when section is not nil, replaces it. The rest of
// the file and its comments are kept. A section that is not there yet reads
// as empty and is created.
func spliceSection(configYAML string, section json.RawMessage) (out string, old json.RawMessage, err error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(configYAML), &doc); err != nil {
		return "", nil, fmt.Errorf("config.yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return "", nil, fmt.Errorf("config.yaml is not a mapping")
	}
	node := doc.Content[0]
	for _, k := range []string{"plugins", "configs"} {
		if node, err = mappingChild(node, k); err != nil {
			return "", nil, err
		}
	}
	old = json.RawMessage("{}")
	if v := mappingValue(node, PluginName); v != nil {
		var cur map[string]any
		if err := v.Decode(&cur); err != nil {
			return "", nil, fmt.Errorf("config.yaml plugins.configs.%s: %w", PluginName, err)
		}
		if cur != nil {
			if old, err = json.Marshal(cur); err != nil {
				return "", nil, fmt.Errorf("config.yaml plugins.configs.%s: %w", PluginName, err)
			}
		}
	}
	if section == nil {
		return configYAML, old, nil
	}
	var repl yaml.Node
	if err := yaml.Unmarshal(section, &repl); err != nil || len(repl.Content) == 0 || repl.Content[0].Kind != yaml.MappingNode {
		return "", nil, fmt.Errorf("section must be an object")
	}
	blockStyle(repl.Content[0])
	if v := mappingValue(node, PluginName); v != nil {
		keepComments(v, repl.Content[0])
		*v = *repl.Content[0]
	} else {
		node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: PluginName}, repl.Content[0])
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", nil, err
	}
	if err := enc.Close(); err != nil {
		return "", nil, err
	}
	return buf.String(), old, nil
}

// mappingValue returns the value of key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// mappingChild returns the mapping under key, creating it if missing.
func mappingChild(m *yaml.Node, key string) (*yaml.Node, error) {
	v := mappingValue(m, key)
	if v == nil {
		v = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	}
	if v.Kind != yaml.MappingNode {
		if v.Tag == "!!null" {
			*v = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		} else {
			return nil, fmt.Errorf("config.yaml: %s is not a mapping", key)
		}
	}
	return v, nil
}

// keepComments copies comments from old onto the keys and values of repl
// that are still there, so notes such as "# Pro" next to a capacity survive.
func keepComments(old, repl *yaml.Node) {
	repl.HeadComment, repl.LineComment, repl.FootComment = old.HeadComment, old.LineComment, old.FootComment
	if old.Kind != yaml.MappingNode || repl.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(repl.Content); i += 2 {
		for j := 0; j+1 < len(old.Content); j += 2 {
			if old.Content[j].Value == repl.Content[i].Value {
				k, ok := repl.Content[i], old.Content[j]
				k.HeadComment, k.LineComment, k.FootComment = ok.HeadComment, ok.LineComment, ok.FootComment
				keepComments(old.Content[j+1], repl.Content[i+1])
			}
		}
	}
}

// blockStyle turns a node parsed from JSON into ordinary YAML: block
// collections and plain scalars where the encoder finds them safe.
func blockStyle(n *yaml.Node) {
	n.Style = 0
	for _, c := range n.Content {
		blockStyle(c)
	}
}
