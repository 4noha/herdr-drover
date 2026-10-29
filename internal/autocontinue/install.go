package autocontinue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Stop フックの設置先（両エージェントとも `hooks.Stop[].hooks[]` の同じ形）。
//
//   - Claude Code: ~/.claude/settings.json  （timeout は秒・既定 60）
//   - Codex:       ~/.codex/hooks.json       （timeout は秒・既定 600。
//     非管理フックは各 PC で一度 `/hooks` から trust しないと走らない＝設置時に報告）
//
// 設置は**追記のみ**（既存のフック・他キーは温存）。自分のエントリは command に
// `herdr-drover stop-hook <agent>` を含むことで同定する（exact-match）。

// HookTimeoutSec はフックの上限秒（判定器 JudgeTimeout より長く）。
const HookTimeoutSec = 55

// HookCommand は設置する command 文字列。bin は herdr-drover の絶対パス。
func HookCommand(bin, agent string) string {
	return fmt.Sprintf("'%s' stop-hook %s", strings.ReplaceAll(bin, "'", `'\''`), agent)
}

// HookMarker は自分のエントリを同定する部分文字列。
func HookMarker(agent string) string { return "herdr-drover' stop-hook " + agent }

// SettingsPath は agent ごとの設定ファイル。
func SettingsPath(home, agent string) (string, error) {
	switch agent {
	case "claude":
		return filepath.Join(home, ".claude", "settings.json"), nil
	case "codex":
		return filepath.Join(home, ".codex", "hooks.json"), nil
	}
	return "", fmt.Errorf("Stop フック未対応のエージェント: %s", agent)
}

// hooksDoc は settings の最小ビュー（他キーは raw に温存）。
type hooksDoc struct {
	raw   map[string]json.RawMessage
	hooks map[string]json.RawMessage // hooks.<Event> → raw array
}

func loadHooksDoc(path string) (*hooksDoc, error) {
	d := &hooksDoc{raw: map[string]json.RawMessage{}, hooks: map[string]json.RawMessage{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return d, nil
	}
	if err := json.Unmarshal(b, &d.raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if h, ok := d.raw["hooks"]; ok && len(h) > 0 {
		if err := json.Unmarshal(h, &d.hooks); err != nil {
			return nil, fmt.Errorf("%s: hooks: %w", path, err)
		}
	}
	return d, nil
}

func (d *hooksDoc) save(path string) error {
	hb, err := json.Marshal(d.hooks)
	if err != nil {
		return err
	}
	d.raw["hooks"] = hb
	if len(d.hooks) == 0 {
		delete(d.raw, "hooks")
	}
	out, err := json.MarshalIndent(d.raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stopGroups は hooks.Stop の配列（matcher group の列）。
func (d *hooksDoc) stopGroups() ([]map[string]any, error) {
	var groups []map[string]any
	if raw, ok := d.hooks["Stop"]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &groups); err != nil {
			return nil, fmt.Errorf("hooks.Stop: %w", err)
		}
	}
	return groups, nil
}

func (d *hooksDoc) setStopGroups(groups []map[string]any) error {
	if len(groups) == 0 {
		delete(d.hooks, "Stop")
		return nil
	}
	b, err := json.Marshal(groups)
	if err != nil {
		return err
	}
	d.hooks["Stop"] = b
	return nil
}

// groupHasMarker は group 内の handler に marker を含む command があるか。
func groupHasMarker(g map[string]any, marker string) (string, bool) {
	hs, _ := g["hooks"].([]any)
	for _, h := range hs {
		hm, _ := h.(map[string]any)
		c, _ := hm["command"].(string)
		if strings.Contains(c, marker) {
			return c, true
		}
	}
	return "", false
}

// Installed は設置済みか（command も返す）。
func Installed(path, agent string) (string, bool, error) {
	d, err := loadHooksDoc(path)
	if err != nil {
		return "", false, err
	}
	groups, err := d.stopGroups()
	if err != nil {
		return "", false, err
	}
	for _, g := range groups {
		if c, ok := groupHasMarker(g, HookMarker(agent)); ok {
			return c, true, nil
		}
	}
	return "", false, nil
}

// Install は Stop フックを追記する。既に同じ command なら何もしない（changed=false）。
// command が違う（bin の場所が変わった等）なら差し替える。
func Install(path, agent, bin string) (changed bool, err error) {
	d, err := loadHooksDoc(path)
	if err != nil {
		return false, err
	}
	groups, err := d.stopGroups()
	if err != nil {
		return false, err
	}
	want := HookCommand(bin, agent)
	handler := map[string]any{"type": "command", "command": want, "timeout": HookTimeoutSec}
	for i, g := range groups {
		c, ok := groupHasMarker(g, HookMarker(agent))
		if !ok {
			continue
		}
		if c == want {
			return false, nil
		}
		groups[i] = map[string]any{"hooks": []any{handler}}
		if err := d.setStopGroups(groups); err != nil {
			return false, err
		}
		return true, d.save(path)
	}
	groups = append(groups, map[string]any{"hooks": []any{handler}})
	if err := d.setStopGroups(groups); err != nil {
		return false, err
	}
	return true, d.save(path)
}

// Uninstall は自分のエントリだけ除く（他は温存）。
func Uninstall(path, agent string) (changed bool, err error) {
	d, err := loadHooksDoc(path)
	if err != nil {
		return false, err
	}
	groups, err := d.stopGroups()
	if err != nil {
		return false, err
	}
	kept := groups[:0]
	for _, g := range groups {
		if _, ok := groupHasMarker(g, HookMarker(agent)); ok {
			changed = true
			continue
		}
		kept = append(kept, g)
	}
	if !changed {
		return false, nil
	}
	if err := d.setStopGroups(kept); err != nil {
		return false, err
	}
	return true, d.save(path)
}
