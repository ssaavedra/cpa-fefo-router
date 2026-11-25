package router

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Making the runtime config permanent: promote builds the new config.yaml
// section and backs up the old one, adopt installs it and ends the runtime
// config, and the backup can be restored the same way.
func TestPromoteRuntimeConfigAndRestoreBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	h := newHarness(t, "state_file: "+path)
	h.seed(0.2, 0.2, 0.2, h.now.Add(72*time.Hour), h.now.Add(96*time.Hour), h.now.Add(48*time.Hour))

	if _, err := h.r.UpdateConfig([]byte(`{"promote":{"current":{}}}`)); err == nil {
		t.Fatal("promote without a runtime config accepted")
	}

	doc := h.r.Config().Doc()
	c := doc.Clients["connect"]
	c.Buckets = []string{"d"}
	doc.Clients["connect"] = c
	doc.SpareFactor = 2.5
	body, _ := json.Marshal(map[string]any{"config": doc})
	if _, err := h.r.UpdateConfig(body); err != nil {
		t.Fatal(err)
	}

	old := fmt.Sprintf(`{"enabled":true,"priority":10,"mode":"active","state_file":%q,"spare_factor":1.5,"min_history":"6h"}`, path)
	v, err := h.r.UpdateConfig([]byte(`{"promote":{"current":` + old + `}}`))
	if err != nil {
		t.Fatal(err)
	}
	var section map[string]any
	if err := json.Unmarshal(v.Section, &section); err != nil {
		t.Fatal(err)
	}
	if section["enabled"] != true || section["priority"] != float64(10) || section["state_file"] != path || section["spare_factor"] != 2.5 {
		t.Fatalf("section keeps host keys and takes routing from the runtime config: %v", section)
	}
	if len(v.Backups) != 1 || string(v.Backups[0].Section) != old {
		t.Fatalf("old section not backed up: %+v", v.Backups)
	}
	if v.Source != "runtime" {
		t.Fatal("promote alone must not end the runtime config")
	}

	promoted := v.Section
	adopt, _ := json.Marshal(map[string]any{"adopt": map[string]any{"section": promoted, "drop_overlay": true}})
	v, err = h.r.UpdateConfig(adopt)
	if err != nil {
		t.Fatal(err)
	}
	if v.Source != "config.yaml" || v.Effective.SpareFactor != 2.5 || !strings.Contains(v.BaseYAML, `"spare_factor":2.5`) {
		t.Fatalf("adopt: source %s spare %.1f", v.Source, v.Effective.SpareFactor)
	}
	h.expectPick("connect", "s1", acctD)

	// Backups survive a restart.
	h.r.saveState()
	h2 := newHarness(t, "state_file: "+path)
	h2.seed(0.2, 0.2, 0.2, h2.now.Add(72*time.Hour), h2.now.Add(96*time.Hour), h2.now.Add(48*time.Hour))
	v = h2.r.ConfigView()
	if len(v.Backups) != 1 {
		t.Fatalf("backups after restart: %+v", v.Backups)
	}

	// Restore: back up what is there now, then adopt the old section.
	restore := v.Backups[0].Section
	h2.advance(time.Minute)
	bk, _ := json.Marshal(map[string]any{"backup": promoted, "note": "before restore"})
	if _, err := h2.r.UpdateConfig(bk); err != nil {
		t.Fatal(err)
	}
	adopt, _ = json.Marshal(map[string]any{"adopt": map[string]any{"section": restore}})
	v, err = h2.r.UpdateConfig(adopt)
	if err != nil {
		t.Fatal(err)
	}
	if v.Effective.SpareFactor != 1.5 || len(v.Backups) != 2 || v.Backups[0].Note != "before restore" {
		t.Fatalf("restore: spare %.1f backups %+v", v.Effective.SpareFactor, v.Backups)
	}
}

func TestConfigBackupsAreBounded(t *testing.T) {
	h := newHarness(t, "")
	for i := range maxConfigBackups + 5 {
		body := fmt.Sprintf(`{"backup":{"spare_factor":%d}}`, i+1)
		if _, err := h.r.UpdateConfig([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	v := h.r.ConfigView()
	if len(v.Backups) != maxConfigBackups || string(v.Backups[0].Section) != fmt.Sprintf(`{"spare_factor":%d}`, maxConfigBackups+5) {
		t.Fatalf("want the newest %d, newest first: %d, first %s", maxConfigBackups, len(v.Backups), v.Backups[0].Section)
	}
	if _, err := h.r.UpdateConfig([]byte(`{"backup":[1]}`)); err == nil {
		t.Fatal("non-object backup accepted")
	}
}
