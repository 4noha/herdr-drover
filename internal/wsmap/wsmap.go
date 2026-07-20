// Package wsmap は claude セッションの「着地ルール」基盤。
// ~/.herdr-drover/workspaces.json に「どの cwd の claude をどの workspace
// （label）へ着地させるか」を持ち、決定的に解決する。
//
// スキーマ（ユーザー明示のルールのみ＝ヒューリスティック分類禁止の鉄則③）:
//
//	{
//	  "exact":   {"/abs/cwd": "label", "~/works/x": "label2"},
//	  "rules":   [{"prefix": "/abs/dir", "workspace": "label"}, ...],
//	  "default": "label"
//	}
//
// 解決順は exact > 最長 prefix > default > ""（=ルール無し）。パスは ~ 展開
// 対応。壊れたファイルは loud にエラー（silent fallback は「設定変更が silent
// に起きる魔法」と同罪＝鉄則④。未知フィールドも typo が黙って無視されると
// ルールが効かない事故になるため loud に拒否する）。
package wsmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// Rule は prefix 1 本の着地ルール。prefix 配下（パス境界一致）の cwd を
// workspace（label）へ着地させる。
type Rule struct {
	Prefix    string `json:"prefix"`
	Workspace string `json:"workspace"`
}

// Map は workspaces.json 全体。フィールドはファイルの生値のまま保持する
// （~ 展開は Resolve 時＝Load→Save でユーザーの書式を silent に書き換えない。
// 鉄則④: 設定変更が silent に起きる魔法禁止）。
type Map struct {
	Exact   map[string]string `json:"exact,omitempty"`
	Rules   []Rule            `json:"rules,omitempty"`
	Default string            `json:"default,omitempty"`
	// InjectPlacement は「リモート pane 注入」の着地ルール（v0.5.5〜）。
	// 形式: {pc: {short_dir: workspace_label}}
	// 例: {"mac-studio-herdr": {"obsidian-vault": "obsidian",
	//                          "audio-router":   "hobby"}}
	// reconcile が CREATE 時に (session.pc, session.short_dir) で引き、
	// マッチすれば label を wsmap.ResolveWorkspaceID で workspace_id に変換して
	// その WS に着地させる。マッチしなければ従来通り InjWorkspaceLabel
	// （デフォルト "↗remote"）へ着地。
	// **exact-match のみ**（prefix / heuristics は使わない＝鉄則③）。
	InjectPlacement map[string]map[string]string `json:"inject_placement,omitempty"`
}

// Path は ~/.herdr-drover/workspaces.json（config.json/agent.pid と同 dir）。
func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("workspaces.json パス解決: %w", err)
	}
	return filepath.Join(home, ".herdr-drover", "workspaces.json"), nil
}

// Load は workspaces.json を読む。ファイル不在は「ルール無し」＝空 Map で
// 正常（エラーでない）。存在するのに読めない/壊れている/検証に落ちるは
// **loud にエラー**（黙って空扱いにするとユーザーのルールが silent に消える）。
func Load() (*Map, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Map{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspaces.json 読取: %w", err)
	}
	m, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return m, nil
}

// Parse はバイト列から Map を厳格に decode＋検証する（Load から分離した
// 純関数＝テーブルテスト対象）。未知フィールドは typo の silent 無視を防ぐ
// ため拒否する。
func Parse(data []byte) (*Map, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Map
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("workspaces.json が壊れている（JSON 不正または未知フィールド）: %w", err)
	}
	// 2 個目の JSON 値が続くファイルも壊れ扱い（連結事故の検出）。
	if dec.More() {
		return nil, fmt.Errorf("workspaces.json が壊れている: JSON 値の後に余分な内容がある")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate はルールの型レベルで判定できる誤りを loud に拒否する。
// パスは「絶対 or ~ 始まり」のみ許可（相対パスは cwd 依存で非決定＝拒否）。
func (m *Map) validate() error {
	for k, v := range m.Exact {
		if !isRulePath(k) {
			return fmt.Errorf("exact のキー %q が絶対パスでも ~ 始まりでもない", k)
		}
		if v == "" {
			return fmt.Errorf("exact[%q] の workspace label が空", k)
		}
	}
	for i, r := range m.Rules {
		if !isRulePath(r.Prefix) {
			return fmt.Errorf("rules[%d].prefix %q が絶対パスでも ~ 始まりでもない", i, r.Prefix)
		}
		if r.Workspace == "" {
			return fmt.Errorf("rules[%d]（prefix %q）の workspace label が空", i, r.Prefix)
		}
	}
	for pc, byDir := range m.InjectPlacement {
		if pc == "" {
			return fmt.Errorf("inject_placement のキー pc が空")
		}
		for dir, label := range byDir {
			if dir == "" {
				return fmt.Errorf("inject_placement[%q] の short_dir が空", pc)
			}
			if label == "" {
				return fmt.Errorf("inject_placement[%q][%q] の workspace label が空", pc, dir)
			}
		}
	}
	return nil
}

// ResolveInject は「リモート pane 注入」pane の着地 label を (pc, short_dir) の
// exact-match で解決する。マッチなしは "" を返す（呼び手が既定の
// InjWorkspaceLabel へフォールバックする）。ヒューリスティックは使わない
// （鉄則③・キー正規化なし＝Save したものが Load でそのまま突き合わされる）。
func (m *Map) ResolveInject(pc, shortDir string) string {
	if m == nil || m.InjectPlacement == nil {
		return ""
	}
	if byDir, ok := m.InjectPlacement[pc]; ok {
		if label, ok := byDir[shortDir]; ok {
			return label
		}
	}
	return ""
}

func isRulePath(p string) bool {
	return p == "~" || strings.HasPrefix(p, "~/") || filepath.IsAbs(p)
}

// Save は Map を原子的に書く（cm WriteStatus 教訓の tmp→rename 流儀。
// truncate 直書きは 0B の瞬間が実観測されている）。organize/capture 側の
// 学習永続化が使う想定＝ユーザー明示操作の結果のみ書くこと（鉄則④）。
func (m *Map) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("workspaces.json encode: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(p, data, 0o644)
}

// Update は Load → mutate → Save を workspaces.json.lock の flock（LOCK_EX）
// 下で行う read-modify-write の原子化プリミティブ。
//
// 根拠（レビュー指摘・実再現済の実競合）: capture（CLI プロセス）と learn
// （agent daemon プロセス）は同一ファイルを読み書きする。素の Load→…→Save
// は「Load 後に他方が書いた分」を stale な Map の全量 Save で巻き戻す
// lost update になる（learn が学習した直後のルールが無警告で古い配置へ
// 逆転する。writeFileAtomic は torn write しか防げない）。書き手は必ず本
// 関数経由で**自分が触るキーだけ**を mutate すること＝競合しても他方の
// 書込が保存される。
//
// mutate は (changed, err) を返す。changed=false / err 時は Save しない
// （無変更の再書込でファイルを汚さない・エラー時に中途半端な状態を書かない）。
// lock は本体と別ファイル（.lock）: Save は tmp→rename で本体の inode を
// 置換するため、本体を flock すると置換後に旧 inode を掴んだ待機者と相互
// 排除できない。
func Update(mutate func(*Map) (changed bool, err error)) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(p+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("workspaces.json lock: %w", err)
	}
	defer lf.Close() // close で flock も解放される
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("workspaces.json flock: %w", err)
	}
	m, err := Load()
	if err != nil {
		return err
	}
	changed, err := mutate(m)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return m.Save()
}

// writeFileAtomic は tmp→rename の原子書込（install.go と同流儀。tmp 名は
// CreateTemp で一意＝並行 writer の固定 tmp 名衝突を避ける）。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Resolve は cwd の着地先 workspace label を決定的に解決する純関数。
// 優先順: exact > 最長 prefix > default > ""（=ルール無し）。
// home は ~ 展開に使う（純関数性のため引数注入。呼び手は os.UserHomeDir）。
//
// 決定性の担保:
//   - exact: JSON のキーは一意だが、~ 展開後に同一パスへ潰れる別表記
//     （"~/x" と "/home/u/x"）があり得る。Go の map 走査順は非決定なので、
//     キーの辞書順で最初に一致したものを採る（＝常に同じ結果）。
//   - prefix: 展開後の prefix が最長のものを採る。同長は rules 配列の
//     先勝ち（配列順はユーザー明示の順序＝決定的）。
//
// 一致は文字列の path 境界一致のみ（/a/b は /a/bc に一致しない）＝
// ヒューリスティック分類ではない。
func (m *Map) Resolve(cwd, home string) string {
	cwd = filepath.Clean(cwd)

	// exact（辞書順で決定的に）
	keys := make([]string, 0, len(m.Exact))
	for k := range m.Exact {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if expandTilde(k, home) == cwd {
			return m.Exact[k]
		}
	}

	// 最長 prefix（同長は先勝ち＝ > でなく >= にしない）
	best := ""
	bestLen := -1
	for _, r := range m.Rules {
		p := expandTilde(r.Prefix, home)
		if hasPathPrefix(cwd, p) && len(p) > bestLen {
			best = r.Workspace
			bestLen = len(p)
		}
	}
	if best != "" {
		return best
	}

	return m.Default // 未設定なら ""（=ルール無し）
}

// expandTilde は "~" / "~/x" を home 配下へ展開し filepath.Clean で正規化する
// （末尾スラッシュ差・"//" 差で不一致になる罠を吸収）。"~user" 形式は
// サポートしない（isRulePath が通すのは "~" と "~/" 始まりのみ）。
func expandTilde(p, home string) string {
	if p == "~" {
		p = home
	} else if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, p[2:])
	}
	return filepath.Clean(p)
}

// hasPathPrefix は path 境界を守る prefix 一致（cwd == prefix か、
// prefix + "/" で始まる）。ルート "/" は全 abs パスに一致する。
func hasPathPrefix(cwd, prefix string) bool {
	if prefix == "/" {
		return strings.HasPrefix(cwd, "/")
	}
	return cwd == prefix || strings.HasPrefix(cwd, prefix+"/")
}

// Caller は herdrapi.Client の Call を満たす最小 interface（seam。実テストは
// 実 Client を渡す＝合成の別経路を作らない）。
type Caller interface {
	Call(method string, params any) (json.RawMessage, error)
}

// ResolveWorkspaceID は label から workspace_id を解決する。
//   - workspace.list の label **exact-match**（鉄則③: 識別子の突合せのみ）。
//   - herdr は label 重複を許容する（Probe 実測）ため、重複時は number 最小
//     を採る＝生成順で最古・server 再起動を跨いでも安定な決定的選択。
//     label を識別子にせず workspace_id を返すのはこのため。
//   - 不在なら workspace.create {label, focus:false} で自動作成（focus 非奪取
//     ＝Probe 実測 params。作成応答の workspace_id を返す）。
func ResolveWorkspaceID(c Caller, label string) (string, error) {
	if label == "" {
		return "", errors.New("wsmap: 空 label は解決できない（呼び手のバグ）")
	}
	raw, err := c.Call("workspace.list", nil)
	if err != nil {
		return "", fmt.Errorf("workspace.list: %w", err)
	}
	var list struct {
		Workspaces []herdrapi.WorkspaceInfo `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", fmt.Errorf("workspace_list decode: %w", err)
	}
	var found *herdrapi.WorkspaceInfo
	for i := range list.Workspaces {
		ws := &list.Workspaces[i]
		if ws.Label != label {
			continue
		}
		if found == nil || ws.Number < found.Number {
			found = ws
		}
	}
	if found != nil {
		return found.WorkspaceID, nil
	}

	raw, err = c.Call("workspace.create", struct {
		Label string `json:"label"`
		Focus bool   `json:"focus"`
	}{label, false})
	if err != nil {
		return "", fmt.Errorf("workspace.create label=%q: %w", label, err)
	}
	var created herdrapi.WorkspaceCreated
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("workspace_created decode: %w", err)
	}
	if created.Workspace.WorkspaceID == "" {
		return "", fmt.Errorf("workspace.create 応答に workspace_id が無い（wire 変化?）")
	}
	return created.Workspace.WorkspaceID, nil
}
