// herdr-drover — herdr セッション群のクラウド同期 CLI。
//
// サブコマンド dispatch のみを持つ薄い入口。実処理は各 cmdXxx（agent.go /
// status.go / nudge.go）へ委譲する。dispatch は run() として io.Writer 注入
// 可能にし、単体テストで実バイナリ経路（引数→分岐→出力→exit code）を
// そのまま検証する（合成の別関数を作らない）。
package main

import (
	"fmt"
	"io"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run は引数 dispatch の本体。戻り値は exit code
// （0=成功 / 1=実行時エラー / 2=使い方エラー・未実装）。
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "agent", "status", "nudge":
		// 現サブコマンドは全て引数なし。黙って無視すると typo（例:
		// `nudge now`）が成功に見えるので明示エラーにする。
		if len(rest) != 0 {
			fmt.Fprintf(stderr, "herdr-drover %s: 余分な引数 %v（引数は取らない）\n", cmd, rest)
			return 2
		}
	}
	switch cmd {
	case "agent":
		if err := cmdAgent(stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "herdr-drover agent: %v\n", err)
			return 1
		}
		return 0
	case "status":
		if err := cmdStatus(stdout); err != nil {
			fmt.Fprintf(stderr, "herdr-drover status: %v\n", err)
			return 1
		}
		return 0
	case "nudge":
		if err := cmdNudge(stdout); err != nil {
			fmt.Fprintf(stderr, "herdr-drover nudge: %v\n", err)
			return 1
		}
		return 0
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "herdr-drover %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "attach", "enroll", "install":
		// DESIGN.md のリポジトリ構成に載る後続フェーズのコマンド。存在は
		// 予約しつつ、未実装は明示エラーで返す（黙って no-op にしない）。
		fmt.Fprintf(stderr, "herdr-drover %s: 未実装（DESIGN.md の後続フェーズ。現在使えるのは agent/status/nudge/version/help）\n", cmd)
		return 2
	default:
		fmt.Fprintf(stderr, "herdr-drover: 未知のサブコマンド %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `herdr-drover %s — herdr セッションのクラウド同期（cm 系 relay/Firestore を共有）

使い方:
  herdr-drover agent      常駐 daemon（launchd から起動。周期 tick＋SIGUSR1 で即時 re-scan）
  herdr-drover status     daemon 生存・herdr 接続・設定の充足を表示
  herdr-drover nudge      稼働中 daemon へ SIGUSR1（herdr plugin events からの即時 re-scan）
  herdr-drover version    バージョン表示
  herdr-drover help       このヘルプ

環境変数（agent/status が参照）:
  GCP_PROJECT                     Firestore の GCP プロジェクト（agent 必須）
  CLOUD_RELAY_URL                 Cloud Run relay の WSS URL（Phase 2 以降で必要）
  GOOGLE_APPLICATION_CREDENTIALS  SA 鍵パス（未設定なら ADC / FIRESTORE_EMULATOR_HOST）
  PC_ID                           端末 id（既定 <hostname 短縮小文字>-herdr。cm agent と同一 id 禁止）
  HERDR_SOCKET_PATH               herdr ndjson API socket（既定 ~/.config/herdr/herdr.sock）
  DROVER_TICK                     producer 周期（Go duration 形式。既定 5s）
  DROVER_IDLE                     Web ターミナル quiescence 自切断の無通信時間（既定 30s）

未実装（後続フェーズ）: attach / enroll / install
`, version)
}
