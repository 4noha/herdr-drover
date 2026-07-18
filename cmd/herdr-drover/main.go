// herdr-drover — herdr セッション群のクラウド同期 CLI。
//
// サブコマンド dispatch のみを持つ薄い入口。実処理は各 cmdXxx（agent.go /
// status.go / nudge.go）へ委譲する。dispatch は run() として io.Writer 注入
// 可能にし、単体テストで実バイナリ経路（引数→分岐→出力→exit code）を
// そのまま検証する（合成の別関数を作らない）。
package main

import (
	"errors"
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
	case "agent", "status", "nudge", "update":
		// これらは引数なし。黙って無視すると typo（例: `nudge now`）が
		// 成功に見えるので明示エラーにする（enroll は引数を取る）。
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
	case "install":
		// install はフラグ（--dry-run/--no-launchctl）を取るので rest を渡す
		//（install.go が flag.FlagSet で解析）。
		if err := cmdInstall(rest, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "herdr-drover install: %v\n", err)
			return 1
		}
		return 0
	case "uninstall":
		if err := cmdUninstall(rest, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "herdr-drover uninstall: %v\n", err)
			return 1
		}
		return 0
	case "enroll":
		// enroll は位置引数＋--relay を取る（rest を渡す）。使い方エラーは
		// errUsage sentinel で exit 2 に振り分ける。
		if err := cmdEnroll(rest, stdout); err != nil {
			fmt.Fprintf(stderr, "herdr-drover enroll: %v\n", err)
			if errors.Is(err, errUsage) {
				return 2
			}
			return 1
		}
		return 0
	case "update":
		if err := cmdUpdate(stdout); err != nil {
			fmt.Fprintf(stderr, "herdr-drover update: %v\n", err)
			return 1
		}
		return 0
	case "claude":
		// claude シム（claudeshim.go）。rest は実 claude へそのまま渡す
		// 引数列なので余分引数チェックはしない。
		if err := cmdClaude(rest, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "herdr-drover claude: %v\n", err)
			return 1
		}
		return 0
	case "organize":
		// organize はフラグ（--capture/--dry-run）を取るので rest を渡す
		//（organize.go が flag.FlagSet で解析）。
		if err := cmdOrganize(rest, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "herdr-drover organize: %v\n", err)
			return 1
		}
		return 0
	case "attach":
		// DESIGN.md のリポジトリ構成に載る後続フェーズのコマンド。存在は
		// 予約しつつ、未実装は明示エラーで返す（黙って no-op にしない）。
		fmt.Fprintf(stderr, "herdr-drover %s: 未実装（DESIGN.md の後続フェーズ。現在使えるのは agent/status/nudge/enroll/update/install/uninstall/version/help）\n", cmd)
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
  herdr-drover install    launchd 常駐を登録（--dry-run / --no-launchctl。ProcessType は焼かない）
  herdr-drover uninstall  launchd 常駐を解除（plist・稼働バイナリ除去。設定とログは残す）
  herdr-drover enroll <code> --relay wss://<host>
                          Web「＋ 端末を追加」のコードで SA 鍵と設定を自動配置
                          （表示コマンドは claude-master 用＝code と --relay を読み替える）
  herdr-drover claude [args...]
                          claude シム（cm start の C案）: server 自動起動＋cwd
                          完全一致の既存 claude セッションへ attach／複数は番号
                          picker（Enter=先頭・n/0=新規・数字=指定）／無し or
                          引数あり(TTY)は新規 pane／引数あり×非 TTY は素の
                          claude へ透過（alias claude='herdr-drover claude'）
  herdr-drover organize [--dry-run]
                          claude セッションを含む Tab を wsmap ルール
                          （exact-cwd > 最長 prefix > default）解決先の
                          Workspace へ整理（単独 Tab は Tab ごと・同居 Tab は
                          claude pane を新 Tab へ切り出し。曖昧は skip＋報告。
                          --dry-run は計画表示のみ）
  herdr-drover organize --capture [--dry-run]
                          現配置（claude cwd → Workspace label）を exact
                          ルールとして wsmap へ保存（書込前に差分表示。
                          複数 workspace に散る cwd は曖昧＝skip＋報告）
  herdr-drover update     selfupdate（GitHub Releases・sha256 検証・原子置換）
  herdr-drover version    バージョン表示
  herdr-drover help       このヘルプ

環境変数（agent/status が参照。enroll 後は ~/.herdr-drover/config.json でも可＝env が優先）:
  GCP_PROJECT                     Firestore の GCP プロジェクト（agent 必須）
  CLOUD_RELAY_URL                 Cloud Run relay の WSS URL（Phase 2 以降で必要）
  GOOGLE_APPLICATION_CREDENTIALS  SA 鍵パス（未設定なら ADC / FIRESTORE_EMULATOR_HOST）
  PC_ID                           端末 id（既定 <hostname 短縮小文字>-herdr。cm agent と同一 id 禁止）
  HERDR_SOCKET_PATH               herdr ndjson API socket（既定 ~/.config/herdr/herdr.sock）
  DROVER_TICK                     producer 周期（Go duration 形式。既定 5s）
  DROVER_IDLE                     Web ターミナル quiescence 自切断の無通信時間（既定 30s）

未実装（後続フェーズ）: attach
`, version)
}
