// Package injectindex は drover agent 内で「どの pane_id が注入 pane で、
// どの (pc,sid) を担っているか」を持つ唯一の権威（single source of truth）。
//
// 背景・設計判断:
//   - 従来は「注入専用 workspace（label==↗remote）所属」が注入判定の権威だった
//     が、ユーザーが mv-tab で別 WS へ動かしたり workspace を rename すると
//     判定が壊れる。判定を workspace label / workspace_id から完全に切り離し、
//     token (`drover_inj_pc` / `drover_inj_sid`) を最終権威にする。
//   - token だけでは 2 つの穴が残る:
//     (a) reconcile が pane を create してから token を付与するまでの race 窓に
//         producer が scan すると token 無しで push＝cross-PC 増殖
//     (b) herdr サーバのみ再起動すると report_metadata token が消える
//         （pane_id は保持されるが token は落ちる・実 herdr 0.7.4 で実測）
//   - 本 index が (a) は「pane を作る直前に Reserve で pending 予約」、(b) は
//     「起動時に pane.list と照合して token 無しの pane_id に token を再表明」で塞ぐ。
//
// スレッドセーフ性: agent プロセス単位で 1 インスタンス。全 API を単 mutex で
// 覆う（entries 数は数十のオーダーで read-heavy でもない）。二重起動は既存の
// pidfile ゲートで排除。
//
// 永続化: ~/.herdr-drover/inject-index.json（config.json / workspaces.json と
// 同 dir）へ tmp→rename の atomic write。書換のたび persist する（書換頻度は
// reconcile tick 単位＝数秒〜数十秒に 1 回で十分小さい）。
package injectindex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Entry は 1 注入 pane。Pending=true は「reconcile が layout.apply で pane を
// 生成した直後で token 付与前」の race 窓状態。producer は Pending / 非 Pending
// を問わず index に載っている pane_id を除外することで race 窓を塞ぐ。
type Entry struct {
	PaneID    string    `json:"pane_id"`
	PC        string    `json:"pc"`
	SID       string    `json:"sid"`
	Pending   bool      `json:"pending,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// file は inject-index.json のスキーマ全体。version は将来のスキーマ変更ガード
// （破壊的変更を silent 追加しないため必ずインクリメントする）。
type file struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// currentVersion は現行スキーマの版番。将来 v2 へ上げるときは version=1 の
// ファイルを loud にエラーにする（silent migration 禁止＝鉄則④）。
const currentVersion = 1

// Index は pane_id → Entry のインメモリマップ＋mutex＋永続化パス。
type Index struct {
	mu   sync.Mutex
	data map[string]Entry // pane_id -> entry
	path string
}

// Open は path の JSON を読み込んで Index を返す。ファイル不在は空 Index。
// 壊れは loud にエラー（silent 空扱い禁止＝ルールが黙って消える事故を避ける・
// wsmap.Load 同流儀）。
func Open(path string) (*Index, error) {
	idx := &Index{data: map[string]Entry{}, path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inject-index.json 読取: %w", err)
	}
	f, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range f.Entries {
		if e.PaneID == "" {
			return nil, fmt.Errorf("%s: pane_id が空の entry", path)
		}
		idx.data[e.PaneID] = e
	}
	return idx, nil
}

// parse はバイト列から file を厳格に decode する（未知フィールドは typo/
// 破壊的変更の silent 無視を防ぐため拒否・wsmap.Parse 同規律）。
func parse(data []byte) (*file, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f file
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("inject-index.json が壊れている（JSON 不正または未知フィールド）: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("inject-index.json が壊れている: JSON 値の後に余分な内容がある")
	}
	if f.Version != currentVersion {
		return nil, fmt.Errorf("inject-index.json の version=%d は非対応（現行 %d）", f.Version, currentVersion)
	}
	return &f, nil
}

// Reserve は「reconcile が pane_id を得た直後・token 付与前」に呼ぶ。
// Pending=true で書き persist する。producer は Pending 期間中もこの pane_id
// を index Snapshot 経由で除外できる＝race 窓を塞ぐ。
//
// 既に同 pane_id で Live entry が居る場合は上書きしない（Commit 済の pane を
// 誤って Pending に戻す事故を避ける）。同 pane_id で Pending が居る場合は
// (pc,sid) 更新のみ（reconcile リトライ想定）。
func (i *Index) Reserve(paneID, pc, sid string) error {
	if paneID == "" || pc == "" || sid == "" {
		return fmt.Errorf("Reserve: paneID/pc/sid のいずれかが空")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if e, ok := i.data[paneID]; ok && !e.Pending {
		// 既に Live。Reserve は no-op（呼び手の post-token 再入は Commit 経路）。
		return nil
	}
	i.data[paneID] = Entry{PaneID: paneID, PC: pc, SID: sid, Pending: true, UpdatedAt: time.Now().UTC()}
	return i.persistLocked()
}

// Commit は token 付与成功後に呼ぶ。Pending を落として Live 状態にする。
// entry が居ない場合は新規作成（AdoptToken 相当・reconcile 経路以外からも
// 使える柔軟性）。
func (i *Index) Commit(paneID, pc, sid string) error {
	if paneID == "" || pc == "" || sid == "" {
		return fmt.Errorf("Commit: paneID/pc/sid のいずれかが空")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.data[paneID] = Entry{PaneID: paneID, PC: pc, SID: sid, Pending: false, UpdatedAt: time.Now().UTC()}
	return i.persistLocked()
}

// Forget は pane を index から取り除く（reconcile が close した / 消滅を検出した）。
// 不在は no-op（冪等）。
func (i *Index) Forget(paneID string) error {
	if paneID == "" {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.data[paneID]; !ok {
		return nil
	}
	delete(i.data, paneID)
	return i.persistLocked()
}

// AdoptToken は起動時 self-heal の (a) 分岐で使う: pane.list に token 付き
// pane が居るが index に無い場合、それを取り込む（Live 状態）。既に Live で
// 一致する entry があれば no-op（persist 節約）。
func (i *Index) AdoptToken(paneID, pc, sid string) error {
	if paneID == "" || pc == "" || sid == "" {
		return fmt.Errorf("AdoptToken: paneID/pc/sid のいずれかが空")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if e, ok := i.data[paneID]; ok && !e.Pending && e.PC == pc && e.SID == sid {
		return nil
	}
	i.data[paneID] = Entry{PaneID: paneID, PC: pc, SID: sid, Pending: false, UpdatedAt: time.Now().UTC()}
	return i.persistLocked()
}

// Get は 1 entry を返す（Snapshot より軽量・reconcile が cur 救済で個別に引く用途）。
func (i *Index) Get(paneID string) (Entry, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	e, ok := i.data[paneID]
	return e, ok
}

// Snapshot は全 entry の読取専用コピーを返す。呼び手は結果を自由に変更してよい
// （in-memory の実体は共有しない）。決定的順序（pane_id 昇順）＝ログ比較容易。
func (i *Index) Snapshot() []Entry {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]Entry, 0, len(i.data))
	for _, e := range i.data {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].PaneID < out[b].PaneID })
	return out
}

// IsInjected は producer の O(1) 除外判定用（Pending / Live 問わず true）。
// 呼び頻度が最も高いパスなので Snapshot コピーを回避して直接 map lookup する。
func (i *Index) IsInjected(paneID string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.data[paneID]
	return ok
}

// Path は open 時のパス（テスト用途）。
func (i *Index) Path() string { return i.path }

// persistLocked は mu 保持中に呼ぶ想定の disk 書込。JSON version:1 で marshal
// して atomic write（tmp→rename）で置換する。0B 瞬間・torn write は起きない。
func (i *Index) persistLocked() error {
	entries := make([]Entry, 0, len(i.data))
	for _, e := range i.data {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].PaneID < entries[b].PaneID })
	f := file{Version: currentVersion, Entries: entries}
	data, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return fmt.Errorf("inject-index.json encode: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(i.path, data, 0o600)
}

// writeFileAtomic は tmp→rename の原子書込（install.go / wsmap.go と同流儀。
// tmp 名は CreateTemp で一意＝並行 writer の固定 tmp 名衝突を避ける）。
// パーミッションは 0600（inject-index に pane_id や remote PC 名が入るので、
// 他ユーザー閲覧を塞ぐ・sa.json と同流儀）。
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
