package main

// notify — Web Push（タスク完了通知）。herdr のネイティブ agent_status
// 検出（internal/session.BuildSessions が working/idle/done/blocked を
// 判定・producer.go が Firestore へ同期）を利用し、working から
// idle/done/blocked への遷移（＝タスクが一段落した瞬間）を検知して
// 登録済みブラウザへ FCM push する。
//
// producer.Producer の PushStatus/DeleteSession（近$0 の核）には一切触れ
// ない：WithOnSessions フックが BuildSessions の結果を副作用専用に渡すだけ
// （internal/session/producer.go 参照）。SA 鍵/push 鍵が無い構成では
// taskNotifier.hc が nil のまま＝常に no-op（既存構成は無影響・後方互換）。

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/4noha/drover-cloud/push"
	"github.com/4noha/drover-cloud/state"
)

// pushTokenStore は taskNotifier が push token を読み書きするのに必要な
// 最小 API（*state.Client が満たす）。テストは real Firestore emulator を
// 起動せず fake で注入する（Firestore 側の実挙動は drover-cloud/state の
// 専用テストが real emulator で担保済み＝ここでは transition 検知ロジック
// だけを軽量に検証する）。
type pushTokenStore interface {
	ListPushTokens(ctx context.Context) ([]string, error)
	DeletePushToken(ctx context.Context, token string) error
}

var _ pushTokenStore = (*state.Client)(nil)

// taskNotifyState は「タスク完了」とみなす遷移先（working から抜けた
// 状態）。unknown は除外＝一時的な検出揺れで誤通知しない。
func taskNotifyState(s string) bool {
	switch s {
	case "idle", "done", "blocked":
		return true
	}
	return false
}

// taskNotifier は working→{idle,done,blocked} 遷移を検知して push する。
// prev は pane_id→直前 agent_status（プロセス内メモリのみ・daemon 再起動で
// リセット＝再起動直後の1回は「以前の状態」が不明なので誤通知しない）。
type taskNotifier struct {
	prev      map[string]string
	hc        *http.Client // nil なら push 無効（SA 鍵が読めなかった等）
	projectID string
	// pcName はこの daemon の PC 識別子（cl.PCName・常に非空＝既定 cfg.PCID。
	// clouds.go LoadClouds 参照）。複数 PC が同じ push token 一覧へ送る構成
	// （オーナーの端末全体が対象）で「どの PC のタスクか」を通知に出すため。
	pcName string
	lg     *log.Logger
	// baseURL は push.Send の FCM エンドポイント上書き（""=本番）。
	// テストが httptest.Server の URL を挿すための seam（push.Send 自体の
	// baseURL 引数と同じ設計）。
	baseURL string
}

// newTaskNotifier は cl.SAKeyPath（既定 ~/.herdr-drover/sa.json）を読んで
// FCM 送信用の認証済み client を作る。鍵が無い/読めない/認証失敗はどれも
// hc=nil のまま返す（push 機能 off で通常運用に無影響＝silent にはしない、
// ログには残す。CLAUDE.md「silent な変更をしない」に対応しつつ、push は
// 任意機能なので daemon 起動自体は継続する）。
func newTaskNotifier(ctx context.Context, cl Cloud, lg *log.Logger) *taskNotifier {
	tn := &taskNotifier{prev: map[string]string{}, projectID: cl.Project, pcName: cl.PCName, lg: lg}
	if cl.SAKeyPath == "" {
		lg.Printf("[push] SA 鍵未設定＝タスク完了 push 通知は無効")
		return tn
	}
	b, err := os.ReadFile(cl.SAKeyPath)
	if err != nil {
		lg.Printf("[push] SA 鍵読取失敗（push 通知は無効）: %v", err)
		return tn
	}
	hc, err := push.NewAuthenticatedClient(ctx, b)
	if err != nil {
		lg.Printf("[push] FCM 認証クライアント作成失敗（push 通知は無効）: %v", err)
		return tn
	}
	tn.hc = hc
	lg.Printf("[push] タスク完了 push 通知 有効")
	return tn
}

// check は 1 tick 分の sessions（session.BuildSessions の結果）を見て、
// pane 毎に working→{idle,done,blocked} の遷移を検知し push する。
// 消滅した pane_id は prev から掃除する（今回 sessions に出ない key を除去。
// 肥大化防止・producer の消滅キー掃除と同じ規律）。
func (tn *taskNotifier) check(ctx context.Context, st pushTokenStore, sessions []map[string]any) {
	cur := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		key, _ := s["key"].(string)
		if key == "" {
			continue
		}
		cur[key] = true
		status, _ := s["agent_status"].(string)
		prev := tn.prev[key]
		tn.prev[key] = status
		if prev != "working" || !taskNotifyState(status) {
			continue
		}
		if tn.hc == nil {
			continue // push 無効でも遷移追跡自体は続ける（有効化後すぐ効くように）
		}
		name, _ := s["window_name"].(string)
		dir, _ := s["short_dir"].(string)
		tn.notify(ctx, st, key, name, dir, status)
	}
	for key := range tn.prev {
		if !cur[key] {
			delete(tn.prev, key)
		}
	}
}

// notify は登録済み全ブラウザへ 1 セッション分の完了通知を送る。
// title は「どのタスクか」が一目でわかるようディレクトリ名（プロジェクト名）
// を優先し、body に PC 名と状態を添える（オーナーは複数 PC を運用しうる＝
// 通知だけで発生元が分かる必要がある）。tag は pcName+key で通知を区別し、
// 同一セッションの連続通知だけ最新1件に集約する（SW/devices.js 参照）。
// UNREGISTERED（失効 token）は DeletePushToken で自己修復する。
func (tn *taskNotifier) notify(ctx context.Context, st pushTokenStore, key, name, dir, status string) {
	toks, err := st.ListPushTokens(ctx)
	if err != nil {
		tn.lg.Printf("[push] ListPushTokens 失敗: %v", err)
		return
	}
	if len(toks) == 0 {
		return
	}
	title := dir
	if title == "" {
		title = name
	}
	if title == "" {
		title = "herdr-drover セッション"
	}
	statusLabel := "タスク完了"
	if status == "blocked" {
		statusLabel = "確認待ち"
	}
	body := statusLabel
	if tn.pcName != "" {
		body = tn.pcName + " · " + statusLabel
	}
	tag := key
	if tn.pcName != "" {
		tag = tn.pcName + ":" + key
	}
	for _, tok := range toks {
		invalid, serr := push.Send(ctx, tn.hc, tn.baseURL, tn.projectID, tok, title, body, tag)
		if serr != nil {
			tn.lg.Printf("[push] send 失敗: %v", serr)
		}
		if invalid {
			if derr := st.DeletePushToken(ctx, tok); derr != nil {
				tn.lg.Printf("[push] 失効 token 削除失敗: %v", derr)
			}
		}
	}
}
