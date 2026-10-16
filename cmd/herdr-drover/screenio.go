package main

// 画面 I/O 命令（read-pane/send-input/interrupt）の実体。agent.go の
// CommandRunner{} リテラルに DoReadPane/DoSendInput/DoInterrupt として配線
// する（drover-cloud-mcp-design.md Phase 2）。restartclaude.go / updateclaude.go
// と同じ「herdr API を直叩きし、要約を Ack detail に残す」パターン。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// screenResultTTL は read-pane の結果（mcp_results）の保存期間
// （drover-cloud-mcp-design.md §3.2「10分」）。
const screenResultTTL = 10 * time.Minute

// screenResultMax は read-pane が結果へ書く本文の上限
// （drover-cloud-mcp-design.md §5.1「上限64KB」）。
const screenResultMax = 64 * 1024

// paneReadSources は herdr の pane.read が受理する source（実測。
// internal/herdrapi/client.go の PaneRead コメント）。drover-cloud-mcp-
// design.md 原案の "visible|scrollback" は実際の herdr API と食い違っていた
// ため、実在する列挙に合わせて訂正する（鉄則1: 推測修正をしない）。
var paneReadSources = map[string]bool{
	"visible": true, "recent": true, "recent_unwrapped": true, "detection": true,
}

// readPaneResultWriter は read-pane の大きい結果の保存先。master=
// scConcrete.PutCommandResult を閉じ込めた関数、slave 等で保存先が無い
// 構成は常に明示エラーを返す関数を渡す（silent に結果を捨てない）。
type readPaneResultWriter func(ctx context.Context, id, text string, truncated bool, ttl time.Duration) error

// tailLines は末尾 n 行だけを返す（herdr の pane.read に行数上限パラメータが
// 無いため daemon 側で切る。drover-cloud-mcp-design.md §5.1「lines≤500」）。
// 戻り値の bool は「切った（元が n 行を超えていた）」こと。
func tailLines(text string, n int) (string, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text, false
	}
	return strings.Join(lines[len(lines)-n:], "\n"), true
}

// newDoReadPane は CommandRunner.DoReadPane の実装を返す。
func newDoReadPane(hcli *herdrapi.Client, put readPaneResultWriter) func(context.Context, string, string, map[string]string) (string, error) {
	return func(ctx context.Context, id, sid string, args map[string]string) (string, error) {
		if sid == "" {
			return "", fmt.Errorf("sid が必須")
		}
		source := args["source"]
		if source == "" {
			source = "visible"
		}
		if !paneReadSources[source] {
			return "", fmt.Errorf("source が不正: %q（visible|recent|recent_unwrapped|detection のいずれか）", source)
		}
		info, err := hcli.PaneRead(sid, source)
		if err != nil {
			return "", fmt.Errorf("pane.read 失敗: %w", err)
		}
		text := strings.TrimRight(info.Text, "\n")
		truncated := info.Truncated
		if n, _ := strconv.Atoi(args["lines"]); n > 0 {
			var lineTrunc bool
			text, lineTrunc = tailLines(text, n)
			truncated = truncated || lineTrunc
		}
		if len(text) > screenResultMax {
			text = text[len(text)-screenResultMax:]
			truncated = true
		}
		if put == nil {
			return "", fmt.Errorf("read-pane の結果保存先が未配線（内部エラー）")
		}
		if err := put(ctx, id, text, truncated, screenResultTTL); err != nil {
			return "", fmt.Errorf("結果保存失敗: %w", err)
		}
		return fmt.Sprintf("画面取得完了（%d bytes, truncated=%v, source=%s）", len(text), truncated, source), nil
	}
}

// newDoSendInput は CommandRunner.DoSendInput の実装を返す。text/keys は
// 排他必須。既定では working 中の pane への入力を拒否する（require_status
// で上書き可。drover-cloud-mcp-design.md §5.2・§6「working 中の pane には
// 打たない」）。
func newDoSendInput(hcli *herdrapi.Client) func(context.Context, string, map[string]string) (string, error) {
	return func(_ context.Context, sid string, args map[string]string) (string, error) {
		if sid == "" {
			return "", fmt.Errorf("sid が必須")
		}
		text, hasText := args["text"]
		keysRaw, hasKeys := args["keys"]
		if hasText == hasKeys {
			return "", fmt.Errorf("text と keys はどちらか一方が必須（排他）")
		}

		requireStatus := args["require_status"]
		if requireStatus == "" {
			requireStatus = "idle|blocked"
		}
		if requireStatus != "any" {
			info, err := hcli.PaneGet(sid)
			if err != nil {
				return "", fmt.Errorf("pane.get 失敗: %w", err)
			}
			allowed := map[string]bool{}
			for _, s := range strings.Split(requireStatus, "|") {
				allowed[s] = true
			}
			if !allowed[info.AgentStatus] {
				return "", fmt.Errorf("agent_status=%s のため拒否（require_status=%s）", info.AgentStatus, requireStatus)
			}
		}

		if hasText {
			// \r 込みの確定入力は pane.send_input だと欠落する実測トラップ
			// （SPEC §5.1）＝pane.send_text＋別送の Enter キーで組み立てる。
			if err := hcli.PaneSendText(sid, text); err != nil {
				return "", fmt.Errorf("pane.send_text 失敗: %w", err)
			}
			submit := args["submit"] != "false" // 既定 true
			if submit {
				if err := hcli.PaneSendKeys(sid, "enter"); err != nil {
					return "", fmt.Errorf("送信確定（enter）失敗: %w", err)
				}
			}
			return fmt.Sprintf("テキスト入力完了（%d bytes, submit=%v）", len(text), submit), nil
		}

		keys := strings.Split(keysRaw, ",")
		if err := hcli.PaneSendKeys(sid, keys...); err != nil {
			return "", fmt.Errorf("pane.send_keys 失敗: %w", err)
		}
		return fmt.Sprintf("キー入力完了（%s）", keysRaw), nil
	}
}

// newDoInterrupt は CommandRunner.DoInterrupt の実装を返す（Ctrl-C 相当）。
// agent_status のゲートは掛けない（working 中のものを止めるのが目的のため）。
func newDoInterrupt(hcli *herdrapi.Client) func(context.Context, string) (string, error) {
	return func(_ context.Context, sid string) (string, error) {
		if sid == "" {
			return "", fmt.Errorf("sid が必須")
		}
		if err := hcli.PaneSendKeys(sid, "C-c"); err != nil {
			return "", fmt.Errorf("pane.send_keys(C-c) 失敗: %w", err)
		}
		return "中断（Ctrl-C）送信完了", nil
	}
}
