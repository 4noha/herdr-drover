// Package autocontinue — セミオート続行（「つづけて」の自動判定）の状態と判定。
//
// # 何をするか
//
// コーディングエージェント（Claude Code / Codex）は**ターンを終える瞬間に Stop フック**
// を呼ぶ（公式機能・両者とも stdin に `last_assistant_message` と
// `stop_hook_active` を含む JSON、stdout に `{"decision":"block","reason":…}` を
// 返すと停止せず reason を次のユーザ入力として続行する。SPEC §2.6）。
// この package はそのフックの中身＝「人にしか答えられない状態か」を判定し、
// 続行なら定型句（既定「つづけて」）を返す。
//
// # 実測根拠（2026-10-29・このMacの実ログ静的解析）
//
//   - ユーザ入力のうち「つづけて」系は Claude 1.8%・Codex 10.5%・opencode 6.2%。
//     Codex は打つまでの待ち中央値 6 分＝人が張り付いて押している。
//   - 直前のエージェント発話は「未完です／次は…」が 4 割、疑問文で終わるのは 1%。
//   - 番号付き選択肢に対してもユーザは「つづけて」で返す＝選択肢の番号を解く必要は
//     ない（エージェントは自分の推奨を知っている）。判定は 2 値で十分。
//
// # 規律（CLAUDE.md 鉄則）
//
//   - silent にしない: 判定・送った文言は毎回 autocontinue.log と画面（reason の
//     `[semi-auto n/cap]` 接頭辞）に出す。
//   - 既定 OFF。pane 単位（`HERDR_PANE_ID`）か全体で明示的に on にする。
//   - 上限（裁量なし 3 回・裁量あり 10 回）で必ず止まる。判定器が落ちたら**停止側**。
//   - 自動続行した停止は push/webhook 通知の対象にしない（suppress マーク。
//     notify.go / webhooknotify.go が参照）。本当に止まった時は通常どおり通知。
package autocontinue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DirEnv は状態ディレクトリの上書き env（テスト・隔離用）。
const DirEnv = "HERDR_DROVER_AUTO_DIR"

// 既定値。
const (
	DefaultPhrase   = "つづけて"
	DefaultCap      = 3
	DefaultGrantCap = 10
	// MarkTTL は suppress マークの有効期間。フック（判定器込み）が終わるまでの
	// 上限より長く、エージェントが kill された等で永久に抑止しない程度に短く。
	MarkTTL = 120 * time.Second
)

// Config は全体設定（<dir>/config.json）。
type Config struct {
	// Enabled は全体の既定（pane 設定が無い pane に適用）。nil=false。
	Enabled *bool `json:"enabled,omitempty"`
	// Phrase は続行時に送る文言の既定。空なら DefaultPhrase。
	Phrase string `json:"phrase,omitempty"`
	// Cap / GrantCap は 1 ユーザ入力あたりの自動続行上限（裁量なし／あり）。0=既定。
	Cap      int `json:"cap,omitempty"`
	GrantCap int `json:"grant_cap,omitempty"`
	// JudgeCmd は判定器コマンド（argv）。空なら DefaultJudgeCmd。
	JudgeCmd []string `json:"judge_cmd,omitempty"`
	// Debug はフック呼出を**全件**（OFF の pane・pane 外も）ログに残す（切り分け用）。
	Debug bool `json:"debug,omitempty"`
}

// PaneSetting は pane 単位の設定（<dir>/panes/<pane>.json）。
type PaneSetting struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Phrase  string `json:"phrase,omitempty"`
	// Grant は「いけるところまで」相当の裁量を明示的に与える（上限 GrantCap）。
	Grant bool   `json:"grant,omitempty"`
	At    string `json:"at,omitempty"`
}

// Counter は agent+session 単位の自動続行回数（<dir>/sessions/<agent>_<sid>.json）。
type Counter struct {
	Count int    `json:"count"`
	At    string `json:"at,omitempty"`
}

// Mark は通知抑止マーク（<dir>/suppress/<pane>.json）。Pending は判定中、
// Continue は「直近の停止は自動続行した」。どちらも通知を抑止する。
type Mark struct {
	At       time.Time `json:"at"`
	Pending  bool      `json:"pending,omitempty"`
	Continue bool      `json:"continue,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// Store は状態ディレクトリへの読み書き。
type Store struct{ Dir string }

// ResolveDir は env > ~/.herdr-drover/autocontinue の順で状態ディレクトリを決める。
func ResolveDir() (string, error) {
	if d := os.Getenv(DirEnv); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home ディレクトリ不明: %w", err)
	}
	return filepath.Join(home, ".herdr-drover", "autocontinue"), nil
}

// Open は状態ディレクトリを解決して Store を返す（ディレクトリはまだ作らない）。
func Open() (*Store, error) {
	d, err := ResolveDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: d}, nil
}

// safeName は pane id（`w1:p4F`）等をファイル名に使える形へ（':' と '/' を '_'）。
func safeName(id string) string {
	r := strings.NewReplacer(":", "_", "/", "_", "\\", "_")
	return r.Replace(id)
}

func (s *Store) configPath() string       { return filepath.Join(s.Dir, "config.json") }
func (s *Store) panePath(p string) string { return filepath.Join(s.Dir, "panes", safeName(p)+".json") }
func (s *Store) counterPath(agent, sid string) string {
	return filepath.Join(s.Dir, "sessions", safeName(agent)+"_"+safeName(sid)+".json")
}
func (s *Store) markPath(p string) string {
	return filepath.Join(s.Dir, "suppress", safeName(p)+".json")
}

// LogPath は判定ログ（1 フック 1 行）。
func (s *Store) LogPath() string { return filepath.Join(s.Dir, "autocontinue.log") }

func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return true, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadConfig は config.json（無ければゼロ値）。
func (s *Store) LoadConfig() (Config, error) {
	var c Config
	_, err := readJSON(s.configPath(), &c)
	return c, err
}

// SaveConfig は config.json を書く。
func (s *Store) SaveConfig(c Config) error { return writeJSON(s.configPath(), c) }

// LoadPane は pane 設定（無ければ found=false）。
func (s *Store) LoadPane(pane string) (PaneSetting, bool, error) {
	var p PaneSetting
	found, err := readJSON(s.panePath(pane), &p)
	return p, found, err
}

// SavePane / DeletePane。
func (s *Store) SavePane(pane string, p PaneSetting) error { return writeJSON(s.panePath(pane), p) }
func (s *Store) DeletePane(pane string) error {
	err := os.Remove(s.panePath(pane))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ListPanes は pane 設定の一覧（pane id → 設定）。
func (s *Store) ListPanes() (map[string]PaneSetting, error) {
	out := map[string]PaneSetting{}
	ents, err := os.ReadDir(filepath.Join(s.Dir, "panes"))
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		var p PaneSetting
		if _, err := readJSON(filepath.Join(s.Dir, "panes", name), &p); err != nil {
			return nil, err
		}
		// ファイル名は safeName 済み（':'→'_'）。元の id は復元できないので
		// 表示用に '_' のまま返す（w1_p4F）。pane id に '_' は使われない前提。
		out[strings.TrimSuffix(name, ".json")] = p
	}
	return out, nil
}

// LoadCounter / SaveCounter。
func (s *Store) LoadCounter(agent, sid string) (Counter, error) {
	var c Counter
	_, err := readJSON(s.counterPath(agent, sid), &c)
	return c, err
}
func (s *Store) SaveCounter(agent, sid string, c Counter) error {
	return writeJSON(s.counterPath(agent, sid), c)
}

// SetMark は suppress マークを書く。ClearMark は消す。
func (s *Store) SetMark(pane string, m Mark) error { return writeJSON(s.markPath(pane), m) }
func (s *Store) ClearMark(pane string) error {
	err := os.Remove(s.markPath(pane))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// LoadMark はマーク（無ければ found=false）。
func (s *Store) LoadMark(pane string) (Mark, bool, error) {
	var m Mark
	found, err := readJSON(s.markPath(pane), &m)
	return m, found, err
}

// Suppressed は「この pane の直近の停止はセミオートが処理中／自動続行した」か。
// MarkTTL を過ぎたマークは無効（エージェントが kill された等で永久に抑止しない）。
func (s *Store) Suppressed(pane string, now time.Time) (bool, Mark) {
	m, found, err := s.LoadMark(pane)
	if err != nil || !found {
		return false, Mark{}
	}
	if now.Sub(m.At) > MarkTTL || m.At.After(now.Add(MarkTTL)) {
		return false, m
	}
	return m.Pending || m.Continue, m
}

// SuppressFunc は notifier に渡す seam（pane id → 抑止中か）。dir が空なら
// 常に false（状態ディレクトリ未解決の構成でも daemon を止めない）。
func SuppressFunc(dir string, now func() time.Time) func(pane string) bool {
	if dir == "" {
		return func(string) bool { return false }
	}
	st := &Store{Dir: dir}
	return func(pane string) bool {
		ok, _ := st.Suppressed(pane, now())
		return ok
	}
}

// AppendLog は autocontinue.log に 1 行追記する（失敗は呼び手へ返すだけ＝
// フック本体の判定を止めない）。
func (s *Store) AppendLog(now time.Time, line string) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.LogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %s\n", now.Format("2006/01/02 15:04:05"), line)
	return err
}

// Effective は pane の有効設定（pane 設定 > 全体設定 > 既定）。
type Effective struct {
	Enabled  bool
	Phrase   string
	Grant    bool
	Cap      int
	GrantCap int
	JudgeCmd []string
}

// Resolve は config と pane 設定を合成する。
func Resolve(c Config, p PaneSetting, paneFound bool) Effective {
	e := Effective{Phrase: DefaultPhrase, Cap: DefaultCap, GrantCap: DefaultGrantCap, JudgeCmd: DefaultJudgeCmd()}
	if c.Enabled != nil {
		e.Enabled = *c.Enabled
	}
	if c.Phrase != "" {
		e.Phrase = c.Phrase
	}
	if c.Cap > 0 {
		e.Cap = c.Cap
	}
	if c.GrantCap > 0 {
		e.GrantCap = c.GrantCap
	}
	if len(c.JudgeCmd) > 0 {
		e.JudgeCmd = c.JudgeCmd
	}
	if paneFound {
		if p.Enabled != nil {
			e.Enabled = *p.Enabled
		}
		if p.Phrase != "" {
			e.Phrase = p.Phrase
		}
		e.Grant = p.Grant
	}
	return e
}
