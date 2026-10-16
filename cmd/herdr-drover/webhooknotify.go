package main

// webhookNotify — セッション状態 Webhook 通知（master）。drover-cloud の
// settings/webhook 設定を読み、agent_status が done/blocked へ**新しく**
// 遷移したセッションだけ非同期で POST する（同じ状態への重複は送らない）。
// notify.go の taskNotifier と同じ設計: producer.Producer の
// PushStatus/DeleteSession（near-$0 の核）には一切触れない。WithOnSessions
// フックが BuildSessions の結果を副作用専用に渡すだけ。
// slave は drover-cloud の web.slavePush が代理送信する（master のみここで
// 送る＝drover-cloud-mcp-design.md の送信元アーキテクチャ決定）。

import (
	"context"
	"log"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/drover-cloud/webhook"
)

// webhookConfigStore は webhookNotifier が設定を読むのに必要な最小 API
// （*state.Client が満たす。notify.go の pushTokenStore と同じ分離理由＝
// テストを fake で軽量に書くため）。
type webhookConfigStore interface {
	GetWebhookConfig(ctx context.Context) (webhook.Config, error)
}

var _ webhookConfigStore = (*state.Client)(nil)

// webhookConfigCacheTTL: 設定読み出しを毎 tick（既定5秒間隔）やると
// Firestore 読みが張り付くため、短時間キャッシュする（owner が設定を
// 変えてから反映されるまでの遅延はこの秒数以内＝許容範囲。near-$0 維持）。
const webhookConfigCacheTTL = 30 * time.Second

// webhookNotifier は agent_status の done/blocked への遷移を検知して送る。
// prev は pane_id→直前 agent_status（プロセス内メモリのみ・daemon 再起動で
// リセット＝再起動直後の1回は「以前の状態」が不明なので誤通知しない。
// taskNotifier と同じ安全側の考え方）。
type webhookNotifier struct {
	prev   map[string]string
	pcName string
	lg     *log.Logger

	cachedCfg webhook.Config
	cachedAt  time.Time
}

func newWebhookNotifier(pcName string, lg *log.Logger) *webhookNotifier {
	return &webhookNotifier{prev: map[string]string{}, pcName: pcName, lg: lg}
}

func (wn *webhookNotifier) configFor(ctx context.Context, st webhookConfigStore) webhook.Config {
	if time.Since(wn.cachedAt) < webhookConfigCacheTTL {
		return wn.cachedCfg
	}
	cfg, err := st.GetWebhookConfig(ctx)
	if err != nil {
		wn.lg.Printf("[webhook] 設定読取失敗（前回値を継続使用）: %v", err)
		return wn.cachedCfg
	}
	wn.cachedCfg, wn.cachedAt = cfg, time.Now()
	return cfg
}

// check は 1 tick 分の sessions（session.BuildSessions の結果）を見て、
// pane 毎に done/blocked への遷移を検知し送る。消滅した pane_id は prev から
// 掃除する（今回 sessions に出ない key を除去。taskNotifier と同じ規律）。
func (wn *webhookNotifier) check(ctx context.Context, st webhookConfigStore, sessions []map[string]any) {
	cur := make(map[string]bool, len(sessions))
	var cfg webhook.Config
	cfgLoaded := false
	for _, s := range sessions {
		key, _ := s["key"].(string)
		if key == "" {
			continue
		}
		cur[key] = true
		status, _ := s["agent_status"].(string)
		prevStatus, known := wn.prev[key]
		wn.prev[key] = status
		if !webhook.NotifyStates[status] || !known || prevStatus == status {
			continue
		}
		if !cfgLoaded {
			cfg = wn.configFor(ctx, st)
			cfgLoaded = true
		}
		if !cfg.Enabled {
			continue
		}
		p := webhook.Payload{
			PC: wn.pcName, SID: key,
			WindowName: strField(s, "window_name"), Agent: strField(s, "agent"),
			Cwd: strField(s, "cwd"), OldStatus: prevStatus, NewStatus: status,
			At: time.Now().UTC().Format(time.RFC3339Nano),
		}
		go func(p webhook.Payload) {
			if err := webhook.Send(context.Background(), cfg, p); err != nil {
				wn.lg.Printf("[webhook] 送信失敗 sid=%s: %v", p.SID, err)
			}
		}(p)
	}
	for key := range wn.prev {
		if !cur[key] {
			delete(wn.prev, key)
		}
	}
}

// strField は session map[string]any から文字列フィールドを安全に取り出す。
func strField(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
