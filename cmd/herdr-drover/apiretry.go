package main

// apiretry — セミオート続行の「API エラーで落ちた停止」の自動再開。
//
// Stop フック（internal/autocontinue）では拾えない型がある: Claude Code は API エラー
// （"API Error: Server error mid-response" 等）で終わるターンでは Stop を呼ばず
// StopFailure になる＝フックが走らない。ユーザーはこれを『すぐ「つづけて」を打てば続く』
// パターンとして扱っている（2026-10-29）。そこで daemon 側で、セミオート ON の pane が
// working→idle/done になった直後に画面末尾を読み、一時的な API エラーの文言なら
// 少し待って「つづけて」を入れる。
//
// 規律（CLAUDE.md 鉄則）:
//   - 対象は**セミオート ON の pane だけ**。判定は画面末尾の exact な文言（推測しない）。
//     レート制限（429・待ち時間あり）は対象外＝すぐ再試行しても効かない。
//   - 連打しない: 待ち時間を 5s→15s→45s と伸ばし、連続 3 回まで。エラー以外の停止や
//     working への復帰で数え直す。
//   - 送る直前に再確認（まだ idle/done・画面に同じエラー・ON のまま）。
//   - 送ったら autocontinue.log と cases.jsonl に残す（silent にしない）。

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/4noha/herdr-drover/internal/autocontinue"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// apiRetryDelays は連続エラーごとの待ち時間（長さ＝最大回数）。
var apiRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second}

// 一時的な API エラーの文言（画面末尾の行に出る形）。429/rate limit は含めない。
var reAPIError = regexp.MustCompile(`(?i)(API Error:?\s*(5\d\d|Server error|Overloaded|Internal server error|Connection error|Request timed out|fetch failed|terminated|socket hang up)|Server error mid-response|overloaded_error|stream (disconnected|ended) before completion|ECONNRESET|upstream connect error|Service Unavailable|\b5\d\d\b[^\n]{0,80}(unable to process your request|Bedrock|Bad Gateway|Gateway Timeout|Service Unavailable)|Retrying in \d+s[^\n]{0,10}attempt \d+/\d+)`)

// reRateLimit は待っても即再開できない型（対象外）。
var reRateLimit = regexp.MustCompile(`(?i)(rate.?limit|usage limit|429|quota|resets? (at|in)|limit reached|credit)`)

// apiErrorLine は画面末尾 n 行から API エラーの行を返す（無ければ ""）。
func apiErrorLine(screen string, n int) string {
	var lines []string
	for _, l := range strings.Split(screen, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if reAPIError.MatchString(lines[i]) {
			if reRateLimit.MatchString(strings.Join(lines, " ")) {
				return ""
			}
			return lines[i]
		}
	}
	return ""
}

type apiRetrier struct {
	mu      sync.Mutex
	prev    map[string]string // pane → 直前の agent_status
	tries   map[string]int    // pane → 連続再開回数
	pending map[string]bool   // pane → 再開待ち goroutine あり

	enabled func(pane string) bool
	read    func(pane string) (string, error)
	send    func(pane, text string) error
	status  func(pane string) string // 現在の agent_status（取れなければ ""）
	after   func(d time.Duration) <-chan time.Time
	record  func(pane, line string)
	lg      *log.Logger
}

func newAPIRetrier(hc *herdrapi.Client, lg *log.Logger) *apiRetrier {
	return &apiRetrier{
		prev: map[string]string{}, tries: map[string]int{}, pending: map[string]bool{},
		enabled: autoSemiAutoEnabled,
		read: func(pane string) (string, error) {
			rd, err := hc.PaneRead(pane, "visible")
			if err != nil {
				return "", err
			}
			return rd.Text, nil
		},
		send: func(pane, text string) error { return hc.PaneSendText(pane, text) },
		status: func(pane string) string {
			pi, err := hc.PaneGet(pane)
			if err != nil {
				return ""
			}
			return pi.AgentStatus
		},
		after: time.After,
		record: func(pane, line string) {
			if st, err := autocontinue.Open(); err == nil {
				_ = st.AppendLog(time.Now(), line)
			}
		},
		lg: lg,
	}
}

// check は producer の 1 tick 分の sessions を見る（notify.go と同じ seam）。
func (a *apiRetrier) check(ctx context.Context, sessions []map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := map[string]bool{}
	for _, s := range sessions {
		key, _ := s["key"].(string)
		if key == "" {
			continue
		}
		cur[key] = true
		status, _ := s["agent_status"].(string)
		prev := a.prev[key]
		a.prev[key] = status
		if status == "working" {
			// 再開が効いた／人が動かした。連続回数は working が一定続いたら戻す
			//（直後の再エラーを数え続けるため、ここでは戻さず stopOK で戻す）。
			continue
		}
		if prev != "working" || (status != "idle" && status != "done") {
			continue
		}
		if a.pending[key] || !a.enabled(key) {
			continue
		}
		a.pending[key] = true
		go a.handle(ctx, key)
	}
	for k := range a.prev {
		if !cur[k] {
			delete(a.prev, k)
			delete(a.tries, k)
			delete(a.pending, k)
		}
	}
}

func (a *apiRetrier) done(pane string) {
	a.mu.Lock()
	delete(a.pending, pane)
	a.mu.Unlock()
}

func (a *apiRetrier) reset(pane string) {
	a.mu.Lock()
	delete(a.tries, pane)
	a.mu.Unlock()
}

// handle は 1 回の停止を見る: 画面の読取りが落ち着くのを待ち、API エラーなら待ち時間後に
// 再確認して「つづけて」を送る。エラーでなければ連続回数を戻す。
func (a *apiRetrier) handle(ctx context.Context, pane string) {
	defer a.done(pane)
	select {
	case <-ctx.Done():
		return
	case <-a.after(2 * time.Second): // 画面の描画が落ち着くのを待つ
	}
	screen, err := a.read(pane)
	if err != nil {
		a.lg.Printf("[api-retry] 画面読取失敗 pane=%s: %v", pane, err)
		return
	}
	line := apiErrorLine(screen, 14)
	if line == "" {
		a.reset(pane) // エラー以外の停止＝連続は途切れた
		return
	}
	a.mu.Lock()
	n := a.tries[pane]
	a.mu.Unlock()
	if n >= len(apiRetryDelays) {
		a.record(pane, fmt.Sprintf("api-retry pane=%s 連続 %d 回再開したが API エラーが続く→停止（人の確認待ち）: %.100s", pane, n, line))
		return
	}
	delay := apiRetryDelays[n]
	select {
	case <-ctx.Done():
		return
	case <-a.after(delay):
	}
	// 送る直前の再確認: まだ止まっている・同じエラー・ON のまま。
	if st := a.status(pane); st != "idle" && st != "done" {
		a.record(pane, fmt.Sprintf("api-retry pane=%s 待機中に状態が %q へ変わった＝再開しない", pane, st))
		return
	}
	if !a.enabled(pane) {
		return
	}
	if again, err := a.read(pane); err != nil || apiErrorLine(again, 14) == "" {
		a.record(pane, fmt.Sprintf("api-retry pane=%s 待機中にエラー表示が消えた＝再開しない", pane))
		return
	}
	text := fmt.Sprintf("[semi-auto api-retry %d/%d] つづけて", n+1, len(apiRetryDelays))
	if err := a.send(pane, text+"\r"); err != nil {
		a.record(pane, fmt.Sprintf("api-retry pane=%s 送信失敗: %v", pane, err))
		return
	}
	a.mu.Lock()
	a.tries[pane] = n + 1
	a.mu.Unlock()
	a.record(pane, fmt.Sprintf("api-retry pane=%s API エラーで停止→%s 後に再開 %q（検出: %.100s）", pane, delay, text, line))
}
