package router

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const cpaConfigYAML = `# CPA config
port: 8317
api-keys: ["sk-secret"]
plugins:
  enabled: true
  configs:
    fefo-router:
      enabled: true
      priority: 10
      mode: active
      state_file: %s
      spare_factor: 1.5 # default
      min_history: 6h
`

// pluginSection reads the fefo-router section of a CPA config.yaml.
func pluginSection(t *testing.T, text string) map[string]any {
	t.Helper()
	var doc struct {
		Plugins struct {
			Configs map[string]map[string]any `yaml:"configs"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Plugins.Configs[PluginName]
}

// Making the runtime config permanent: promote answers with the config.yaml
// to write and backs up the old section, adopt installs it and ends the
// runtime config, and restore puts the backup back.
func TestPromoteRuntimeConfigAndRestoreBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	h := newHarness(t, "state_file: "+path)
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))
	file := fmt.Sprintf(cpaConfigYAML, path)
	req := func(r *Router, v any) ConfigView {
		t.Helper()
		body, _ := json.Marshal(v)
		out, err := r.UpdateConfig(body)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	body, _ := json.Marshal(map[string]any{"promote": map[string]any{"config_yaml": file}})
	if _, err := h.r.UpdateConfig(body); err == nil {
		t.Fatal("promote without a runtime config accepted")
	}

	doc := h.r.Config().Doc()
	c := doc.Clients["connect"]
	c.Buckets = []string{"d"}
	doc.Clients["connect"] = c
	doc.SpareFactor = 2.5
	req(h.r, map[string]any{"config": doc})

	v := req(h.r, map[string]any{"promote": map[string]any{"config_yaml": file}})
	sec := pluginSection(t, v.ConfigYAML)
	if sec["enabled"] != true || sec["priority"] != 10 || sec["state_file"] != path || sec["spare_factor"] != 2.5 {
		t.Fatalf("section keeps host keys and takes routing from the runtime config: %v", sec)
	}
	if !strings.Contains(v.ConfigYAML, "# CPA config") || !strings.Contains(v.ConfigYAML, "sk-secret") || !strings.Contains(v.ConfigYAML, "port: 8317") {
		t.Fatalf("the rest of config.yaml must be kept:\n%s", v.ConfigYAML)
	}
	if len(v.Backups) != 1 {
		t.Fatalf("old section not backed up: %+v", v.Backups)
	}
	var old map[string]any
	_ = json.Unmarshal(v.Backups[0].Section, &old)
	if old["spare_factor"] != 1.5 || old["priority"] != float64(10) {
		t.Fatalf("backup is the old section: %v", old)
	}
	if v.Source != "runtime" {
		t.Fatal("promote alone must not end the runtime config")
	}

	promotedYAML := v.ConfigYAML
	v = req(h.r, map[string]any{"adopt": map[string]any{"section": v.Section, "drop_overlay": true}})
	if v.Source != "config.yaml" || v.Effective.SpareFactor != 2.5 {
		t.Fatalf("adopt: source %s spare %.1f", v.Source, v.Effective.SpareFactor)
	}
	h.expectPick("connect", "s1", acctD)

	// Backups survive a restart.
	h.r.saveState()
	h2 := newHarness(t, "state_file: "+path)
	h2.seed(0.2, 0.2, 0.2, h2.now.Add(72*time.Hour), h2.now.Add(96*time.Hour), h2.now.Add(48*time.Hour))
	h2.advance(time.Minute)
	v = h2.r.ConfigView()
	if len(v.Backups) != 1 {
		t.Fatalf("backups after restart: %+v", v.Backups)
	}

	v = req(h2.r, map[string]any{"restore": map[string]any{"at": v.Backups[0].At, "config_yaml": promotedYAML}})
	if sec := pluginSection(t, v.ConfigYAML); sec["spare_factor"] != 1.5 {
		t.Fatalf("restored section: %v", sec)
	}
	if len(v.Backups) != 2 || !strings.HasPrefix(v.Backups[0].Note, "before restoring") {
		t.Fatalf("restore backs up the current section first: %+v", v.Backups)
	}
	v = req(h2.r, map[string]any{"adopt": map[string]any{"section": v.Section}})
	if v.Effective.SpareFactor != 1.5 {
		t.Fatalf("restore: spare %.1f", v.Effective.SpareFactor)
	}

	body, _ = json.Marshal(map[string]any{"restore": map[string]any{"at": h2.now.Add(time.Hour), "config_yaml": file}})
	if _, err := h2.r.UpdateConfig(body); err == nil {
		t.Fatal("restore of a missing backup accepted")
	}
}

func TestSpliceSectionCreatesMissingSection(t *testing.T) {
	out, old, err := spliceSection("port: 8317\n", json.RawMessage(`{"mode":"active","spare_factor":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(old) != "{}" {
		t.Fatalf("old section of a file without one: %s", old)
	}
	if sec := pluginSection(t, out); sec["mode"] != "active" || sec["spare_factor"] != 2 {
		t.Fatalf("section not created:\n%s", out)
	}
	if strings.Contains(out, "{") {
		t.Fatalf("section must be written as block YAML:\n%s", out)
	}
}

func TestConfigBackupsAreBounded(t *testing.T) {
	h := newHarness(t, "")
	h.r.mu.Lock()
	for i := range maxConfigBackups + 5 {
		h.r.backupSectionLocked(json.RawMessage(fmt.Sprintf(`{"spare_factor":%d}`, i+1)), "")
	}
	h.r.mu.Unlock()
	v := h.r.ConfigView()
	if len(v.Backups) != maxConfigBackups || string(v.Backups[0].Section) != fmt.Sprintf(`{"spare_factor":%d}`, maxConfigBackups+5) {
		t.Fatalf("want the newest %d, newest first: %d, first %s", maxConfigBackups, len(v.Backups), v.Backups[0].Section)
	}
}
