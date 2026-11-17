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
	"unicode/utf8"

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

// tailBytesUTF8Safe は末尾 maxBytes bytes 以内に収める（herdr-drover-mcp-
// design.md §5.1「上限64KB」）。単純な byte slice だと UTF-8 のマルチバイト
// 文字（日本語等）の途中で切れて文字が壊れうるため、有効なルーン境界まで
// 前進させる（最大3バイト。unicode/utf8.RuneStart）。
func tailBytesUTF8Safe(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	cut := len(text) - maxBytes
	for cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut++
	}
	return text[cut:], true
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
		if bt, cut := tailBytesUTF8Safe(text, screenResultMax); cut {
			text = bt
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

// bracketedPasteStart / bracketedPasteEnd は xterm の bracketed paste の印。端末は
// 人間の貼り付けをこれで包んで渡す（TUI はこの印で「貼り付け」と判定する）。
//
// なぜ send-input がこれを使うか（2026-11-17 実測・screenio_live_test.go）:
// herdr の pane.send_text は素のバイト列を PTY に書く。macOS の PTY は読み手に
// **1022 バイトずつ**渡す（TTYHOG 1024。8KB は 1022×7＋846 の 8 塊が 0.1ms 以内に届く）。
// Claude Code 2.1.326 の usePasteHandler は塊ごとに「800 文字超＝貼り付け・未満＝
// 打鍵」と判定し（PASTE_THRESHOLD=800・PASTE_COMPLETION_TIMEOUT_MS=100）、
// 最後の塊が 800 文字未満だと**その塊だけが打鍵経路**に流れる。結果:
//   - 直後の Enter は 100ms の paste 窓に吸われて改行になる（1.5KB/5.5KB/8KB で 0/3 送信）
//   - Enter を待っても、末尾の塊だけが別メッセージとして送信される（1.5KB は 800ms
//     待っても nonce 入りの本文が届かず、末尾 478B の断片だけが 4 回送られた）
//
// 印で包めば塊の境界に関係なく全体が 1 つの貼り付けになり、45B〜8KB × 待ち 0/150/300ms
// の 15 試行すべてが全文 1 メッセージで送信された（断片 0）。
//
// 貼り付け経路でも改行は改行のまま入る（打鍵経路の \n は Enter＝途中送信になる TUI が
// ある）ので、1 行の短文にも常に付ける。bracketed paste は xterm 由来の標準で、
// zsh/bash(readline)/Claude Code/crossterm 系 TUI が同じ印を解釈する。
const (
	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
)

// submitSettle は貼り付けの後 Enter を打つまでの間。実測では 0ms でも送信されたが、
// 遅い PC で Enter が貼り付け処理に追い越されないための余裕（遅延は Ack に載る）。
const submitSettle = 150 * time.Millisecond

// submitVerifyWait は Enter の後、agent_status が入力前の値から変わる（idle/done/
// blocked → working）のを待つ上限。herdr の検出は非同期で ~0.8s 遅れる（CLAUDE.md）。
// 変わらなくても **Enter は再送しない**（Enter は冪等でない: 既に送信済みで承認
// ダイアログが出ていれば再送が承認になる）。結果は Ack detail に「未確認」と残し、
// 呼び手（MCP の AI）が read_pane で確かめられるようにする。
const submitVerifyWait = 3 * time.Second

// submitVerifyPoll は上の待ちの間に agent_status を見る間隔。
const submitVerifyPoll = 250 * time.Millisecond

// sendInputRequireStatusDefault は send-input が既定で入力を許す agent_status。
// idle/blocked に加えて **done**（応答を終えて入力待ち＝idle と同じく安全）を含める。
// 2026-11-17 時点の Firestore 監査で send-input のエラー 17 件中 9 件が
// 「agent_status=done のため拒否」だった＝呼び手には「送信されない」と同じに見える。
const sendInputRequireStatusDefault = "idle|blocked|done"

// sendInputDeps は newDoSendInput が herdr API に触る口。テストで差し替える seam
// （実機は depsFromHerdr）。
type sendInputDeps struct {
	sendText func(pane, text string) error
	sendKeys func(pane string, keys ...string) error
	paneGet  func(pane string) (*herdrapi.PaneInfo, error)
	sleep    func(time.Duration)
	now      func() time.Time
}

func depsFromHerdr(hcli *herdrapi.Client) sendInputDeps {
	return sendInputDeps{
		sendText: hcli.PaneSendText,
		sendKeys: hcli.PaneSendKeys,
		paneGet:  hcli.PaneGet,
		sleep:    time.Sleep,
		now:      time.Now,
	}
}

// newDoSendInput は CommandRunner.DoSendInput の実装を返す。text/keys は
// 排他必須。既定では working 中の pane への入力を拒否する（require_status
// で上書き可。drover-cloud-mcp-design.md §5.2・§6「working 中の pane には
// 打たない」）。
func newDoSendInput(hcli *herdrapi.Client) func(context.Context, string, map[string]string) (string, error) {
	return newDoSendInputWith(depsFromHerdr(hcli))
}

func newDoSendInputWith(d sendInputDeps) func(context.Context, string, map[string]string) (string, error) {
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
			requireStatus = sendInputRequireStatusDefault
		}
		before := "" // 入力前の agent_status（送信確認の基準。any のときも読む）
		if info, err := d.paneGet(sid); err != nil {
			return "", fmt.Errorf("pane.get 失敗: %w", err)
		} else {
			before = info.AgentStatus
		}
		if requireStatus != "any" {
			allowed := map[string]bool{}
			for _, s := range strings.Split(requireStatus, "|") {
				allowed[s] = true
			}
			if !allowed[before] {
				return "", fmt.Errorf("agent_status=%s のため拒否（require_status=%s）", before, requireStatus)
			}
		}

		if hasText {
			// bracketed paste で包む（理由は bracketedPasteStart のコメント）。本文に印が
			// 混ざっていると途中で貼り付けが閉じて残りが打鍵になるので除く（silent に
			// しない＝detail に残す）。\r 込みの確定入力は pane.send_input だと欠落する
			// 実測トラップ（SPEC §5.1）＝pane.send_text＋別送の Enter キーで組み立てる。
			clean := stripPasteMarkers(text)
			note := ""
			if clean != text {
				note = ", 本文中の paste 印を除去"
			}
			if err := d.sendText(sid, bracketedPasteStart+clean+bracketedPasteEnd); err != nil {
				return "", fmt.Errorf("pane.send_text 失敗: %w", err)
			}
			submit := args["submit"] != "false" // 既定 true
			if !submit {
				return fmt.Sprintf("テキスト入力完了（%d bytes, submit=false, bracketed-paste%s）", len(clean), note), nil
			}
			d.sleep(submitSettle)
			if err := d.sendKeys(sid, "enter"); err != nil {
				return "", fmt.Errorf("送信確定（enter）失敗: %w", err)
			}
			return fmt.Sprintf("テキスト入力完了（%d bytes, submit=true, bracketed-paste%s, 送信確認=%s）",
				len(clean), note, verifySubmit(d, sid, before)), nil
		}

		keys := strings.Split(keysRaw, ",")
		if err := d.sendKeys(sid, keys...); err != nil {
			return "", fmt.Errorf("pane.send_keys 失敗: %w", err)
		}
		return fmt.Sprintf("キー入力完了（%s）", keysRaw), nil
	}
}

// stripPasteMarkers は本文に紛れた bracketed paste の印を除く（送る側が付ける印は
// newDoSendInputWith が外側に付ける）。
func stripPasteMarkers(text string) string {
	return strings.NewReplacer(bracketedPasteStart, "", bracketedPasteEnd, "").Replace(text)
}

// verifySubmit は Enter の後に agent_status が before から working へ変わったかを
// submitVerifyWait まで見て、Ack detail 用の短い説明を返す。入力前が working/unknown
// なら変化で判定できないので省略と書く。Enter の再送はしない（submitVerifyWait の
// コメント）。
func verifySubmit(d sendInputDeps, sid, before string) string {
	if before == "working" || before == "unknown" || before == "" {
		return fmt.Sprintf("省略（入力前 agent_status=%s）", before)
	}
	start := d.now()
	for {
		info, err := d.paneGet(sid)
		if err == nil && info.AgentStatus != before && info.AgentStatus == "working" {
			return fmt.Sprintf("%s→working %s", before, d.now().Sub(start).Round(100*time.Millisecond))
		}
		if d.now().Sub(start) >= submitVerifyWait {
			cur := before
			if err == nil {
				cur = info.AgentStatus
			}
			return fmt.Sprintf("未確認（agent_status=%s のまま %s。入力欄に残っていれば read_pane で見える。Enter は再送しない）", cur, submitVerifyWait)
		}
		d.sleep(submitVerifyPoll)
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
