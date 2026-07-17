package main

// agent — 常駐 daemon（launchd から起動。ProcessType=Background は禁止＝
// cm STATUS flap 事故の教訓: throttle された scan が timeout → 空同期の
// 正帰還になる）。
//
// 役割は Phase 1 の producer 駆動のみ:
//   設定解決 → pidfile → herdr ping（version/protocol 確認）→ Firestore
//   接続 → RegisterPC → producer ループ（周期 tick＋SIGUSR1 即時 re-scan）
//   → SIGTERM/SIGINT で graceful 終了。

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/4noha/herdr-drover/internal/cloud/state"
	"github.com/4noha/herdr-drover/internal/commands"
	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/selfupdate"
	"github.com/4noha/herdr-drover/internal/session"
)

// restartSelf は launchd 配下の自分を kickstart -k で強制再起動する
// （cm restartDaemons の drover 版＝単一 agent なので 1 本だけ）。
// ラベルは install.go の launchdLabel（plist を書く側と同一定数＝不一致で
// 遠隔再起動が空振りする事故を構造的に防ぐ）。launchd 外での実行（開発時
// の手動 agent）では失敗するが、CommandRunner が Ack 先行済みなので監査は
// done で残る（cm と同じ規律）。
func restartSelf() error {
	dom := fmt.Sprintf("gui/%d", os.Getuid())
	return exec.Command("launchctl", "kickstart", "-k", dom+"/"+launchdLabel).Run()
}

// knownProtocol は実測で挙動検証済みの herdr ndjson protocol
// （v0.7.4 の ping 応答＝herdrapi パッケージの採取値）。
// DESIGN の hidden CLI リスク対応: 未知 protocol は起動拒否ではなく
// **警告して継続**（herdr 側の後方互換に期待しつつ、挙動差の一次容疑者を
// ログに残す）。exact-match 比較のみ＝ヒューリスティック判定はしない。
const knownProtocol = 16

func cmdAgent(stdout, stderr io.Writer) error {
	lg := log.New(stderr, "", log.LstdFlags)

	cfg, err := resolveConfig()
	if err != nil {
		return err
	}
	if cfg.Project == "" {
		return fmt.Errorf("GCP_PROJECT が未設定（agent は Firestore 同期が本体＝必須）")
	}
	for _, w := range warnConfig(cfg) {
		lg.Println(w)
	}

	// 二重起動ゲート: flock をプロセス生存期間中保持する（check-then-write
	// の TOCTOU 排除。詳細と実測根拠は pidfile.go の acquirePidfile コメント）。
	pidPath, err := pidfilePath()
	if err != nil {
		return err
	}
	lock, err := acquirePidfile(pidPath, os.Getpid())
	if err != nil {
		return err
	}
	defer lock.Close()       // LIFO＝pidfile 掃除の後に lock 解放
	defer os.Remove(pidPath) // graceful 終了時のみ。SIGKILL 時は stale が残る（読む側の pidAlive が担保）

	// herdr ping: 接続不能は即エラー終了（launchd KeepAlive が再起動する。
	// ここで retry ループを持つより、素直に落ちて supervision に任せる方が
	// 状態が単純）。version/protocol は起動ログに必ず残す＝障害調査の一次情報。
	hcli := herdrapi.New(cfg.SocketPath)
	pong, err := hcli.Ping()
	if err != nil {
		return fmt.Errorf("herdr へ接続できない（socket=%s）: %w", cfg.SocketPath, err)
	}
	lg.Printf("herdr ok: version=%s protocol=%d (socket=%s)", pong.Version, pong.Protocol, cfg.SocketPath)
	for _, w := range verifyHerdr(pong) {
		lg.Println(w)
	}

	// graceful 終了（SIGTERM/SIGINT）と nudge（SIGUSR1）は別チャネル:
	// 前者は ctx cancel、後者は producer ループの即時 re-scan トリガ。
	//
	// 順序メモ（レビュー指摘「pidfile 書込〜Notify の窓で SIGUSR1 を受けると
	// 既定動作で agent が即死する」の検証結果＝棄却）: Go runtime は起動時に
	// 全 _SigNotify シグナルへ自前ハンドラを入れ、Notify 未登録の SIGUSR1 は
	// 黙って捨てる＝プロセスは死なない（sigtable は SIGUSR1=_SigNotify のみ・
	// go1.20〜1.26 で確認。os/signal doc も "signals that otherwise cause no
	// action: SIGUSR1..."。実 agent＋応答しない socket で ping ブロック中＝
	// この窓の真っ只中に実 SIGUSR1 を撃ち、生存を実測した）。窓で失われた
	// nudge は runAgentLoop が起動直後に必ず 1 回 tick するので取りこぼしも
	// ない。よって登録順の移動はしない。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	nudge := make(chan os.Signal, 1)
	signal.Notify(nudge, syscall.SIGUSR1)
	defer signal.Stop(nudge)

	// 資格情報はクライアント個別に注入する（enroll が書く設定ファイル由来の
	// SA 鍵パスはプロセス env に無い＝ADC では見えないため。env
	// GOOGLE_APPLICATION_CREDENTIALS 由来でも同じ鍵を指すだけで等価）。
	// エミュレータ時は資格情報を渡さない（不要かつ client 生成オプションの
	// 非互換を踏まないため。e2e はこの経路）。
	creds := cfg.Credentials
	if os.Getenv("FIRESTORE_EMULATOR_HOST") != "" {
		creds = ""
	}
	st, err := state.NewWithCredentials(ctx, cfg.Project, cfg.PCID, creds)
	if err != nil {
		return fmt.Errorf("Firestore 接続失敗（GCP_PROJECT=%s）: %w", cfg.Project, err)
	}
	defer st.Close()

	// 起動時 1 回の親 doc 登録（session ゼロの idle PC も Web 一覧に出す
	// ＋agent 版可視化。cm RegisterPCVersion と同じ near-$0 規律）。
	// ここの失敗は資格情報/プロジェクト設定の誤りがほぼ確定なので、
	// 黙って tick で失敗し続けるより fail-fast で原因を露出する。
	//
	// ただし強制失効済なら登録しない（cm runOneCloud と同じ規律。レビュー
	// 指摘で cm parity 欠落と確定）: owner が Web「端末ペアリング解除」
	//（SetRevoked＋DeletePCByID）をした端末が launchd KeepAlive で再起動
	// してきても、再登録せずドーマントで待つ。無視して Register/Push を
	// 続けると消したはずの端末が一覧に蘇り続ける削除合戦になる（旧コードで
	// 実エミュレータ再現済み。回帰: test/ TestE2ERevokedAgentDormant）。
	if st.IsSelfRevoked(ctx) {
		lg.Printf("この端末（pc=%s）はペアリング解除済（dormant）。再 enroll / 失効解除で復帰します", cfg.PCID)
	} else if err := st.RegisterPCVersion(ctx, version); err != nil {
		return fmt.Errorf("RegisterPC 失敗（資格情報/プロジェクト設定を確認）: %w", err)
	}
	lg.Printf("agent 開始: pc=%s project=%s tick=%s version=%s", cfg.PCID, cfg.Project, cfg.Tick, version)

	// Phase 2 Web ターミナル: CLOUD_RELAY_URL 設定時のみ制御線（WatchWake）
	// を起動する（未設定なら Phase 1 一覧同期のみ＝warnConfig が案内済み）。
	// SIGTERM は ctx cancel → WatchWake/全 bridge 停止 → 下の drain で
	// bounded 待ち（配線と規律は webterm.go）。
	var wt *webTerm
	if cfg.RelayURL != "" {
		wt = newWebTerm(cfg.RelayURL, cfg.Idle, st, hcli, lg)
		wt.start(ctx)
		lg.Printf("webterm: WatchWake 起動（relay=%s）", cfg.RelayURL)
	}

	// 遠隔命令制御線（cm runOneCloud と同型の常時・無料 listener。owner
	// 限定は web 側・多層防御の revocation 再検査は CommandRunner 内）。
	// 写像（DESIGN「遠隔命令」）:
	//   restart-agent → launchctl kickstart -k（自己。Ack 先行）
	//   self-update   → selfupdate.Update → Ack 先行 → os.Exit(0)
	//                   （launchd KeepAlive が新バイナリで再起動。graceful
	//                    drain を通らないのは意図＝pidfile は flock 自動解放
	//                    ＋読み手の pidAlive が stale を担保する既定の設計）
	//   restart-proxy → 当該 sid の bridge respawn（webterm）。Web ターミナル
	//                   無効（wt=nil）や未知 sid は status=error で Ack
	cr := &commands.CommandRunner{
		St:        st,
		DoRestart: func(context.Context) error { return restartSelf() },
		DoUpdate: func(context.Context) (string, bool, error) {
			return selfupdate.Update(version)
		},
		DoExit: func() { os.Exit(0) },
	}
	if wt != nil {
		cr.DoProxy = func(_ context.Context, sid string) error {
			return wt.respawn(sid)
		}
	}
	go func() {
		if err := cr.Run(ctx); err != nil && ctx.Err() == nil {
			lg.Printf("command watcher 終了: %v", err)
		}
	}()

	// producer 契約（internal/session と統合済み。シグネチャ不整合は
	// コンパイルで露見する方針＝ここを黙ってスタブで満たさない。実バイナリ
	// の一気通貫〔起動→pane 作成→doc 出現→pane 終了→doc 消滅→SIGTERM
	// graceful〕は test/e2e_test.go の Phase 1 gate が常設検証する）:
	//
	//   session.NewProducer(hcli, st) → prod / prod.Tick(ctx) error
	//
	//   Tick の意味論（DESIGN「一覧同期」＋cm 教訓）:
	//     - 1 tick = pane.list/agent.list → session map（key=pane_id・
	//       short_dir=cwd 末尾・window_name=agent/label・is_active=AgentStatus）
	//       → st.PushStatus（content_hash ゲート＝near-$0）
	//     - 消滅キーは前 tick との in-memory 差分で st.DeleteSession（初回
	//       prev は st.OwnSessionKeys で seed＝agent 再起動跨ぎの取りこぼし防止）
	//     - herdr scan エラー時は Push/Delete を一切せず error を返すこと。
	//       呼び手（本ループ）はその tick を skip＝前回状態維持（cm の
	//       「空 STATUS flap」事故の教訓: エラー時に空で全置換しない）
	prod := session.NewProducer(hcli, st)

	// tick 冒頭の失効検査（cm runOneCloud の producer ループと同じ規律・
	// 同じ頻度＝tick 毎 1 read で near-$0 規律内）: 失効中は scan/push/
	// delete を一切せず doc を再作成しない。owner が ClearRevoked（再
	// enroll）したら次 tick で自然復帰する。ログは遷移時のみ（dormant 中に
	// 同文を tick 毎に吐かない＝runAgentLoop のエラーログと同じ流儀）。
	dormant := false
	tickFn := func(ctx context.Context) error {
		if st.IsSelfRevoked(ctx) {
			if !dormant {
				lg.Printf("ペアリング解除を検出（dormant へ移行・push/delete 停止）")
				dormant = true
			}
			return nil
		}
		if dormant {
			lg.Printf("失効解除を検出（同期再開）")
			dormant = false
		}
		return prod.Tick(ctx)
	}

	runAgentLoop(ctx, cfg.Tick, nudge, tickFn, lg)
	if wt != nil {
		wt.drain(3 * time.Second)
	}
	lg.Printf("agent 終了（graceful）")
	return nil
}

// verifyHerdr は ping 応答の protocol を実測済み値と exact-match 照合し、
// 差異があれば警告文を返す（純関数＝単体テスト対象）。
func verifyHerdr(pong *herdrapi.PongInfo) []string {
	var ws []string
	if pong.Protocol != knownProtocol {
		ws = append(ws, fmt.Sprintf("⚠ 未検証の herdr protocol=%d（検証済は %d・version=%s）。継続するが挙動差に注意（DESIGN: hidden CLI リスク）", pong.Protocol, knownProtocol, pong.Version))
	}
	return ws
}

// runAgentLoop は producer ループ本体。起動直後に 1 回 tick し、以後は
// 周期 tick か nudge（SIGUSR1）の早い方で re-scan する。ctx 終了で戻る。
//
// tick エラーは skip＝前回状態維持（tickFn 側が Push しない契約）。ログは
// 遷移時のみ（エラー発生時と復帰時）: herdr 停止中に 5s 毎の同文ログで
// launchd ログを埋めない。
//
// チャネル注入（nudge）と関数注入（tickFn）で、シグナル/実 producer 抜きに
// 「nudge で即時 re-scan が走る」を決定論に単体テストできる。
func runAgentLoop(ctx context.Context, tick time.Duration, nudge <-chan os.Signal, tickFn func(context.Context) error, lg *log.Logger) {
	t := time.NewTicker(tick)
	defer t.Stop()
	lastErrMsg := "" // 直前 tick のエラー文（"" は正常）＝遷移検出用
	for {
		if err := tickFn(ctx); err != nil {
			if ctx.Err() != nil {
				return // 終了要求由来のエラーはログしない
			}
			if msg := err.Error(); msg != lastErrMsg {
				lg.Printf("tick エラー（skip＝前回状態維持）: %v", err)
				lastErrMsg = msg
			}
		} else if lastErrMsg != "" {
			lg.Printf("tick 復帰（エラー解消）")
			lastErrMsg = ""
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-nudge:
			// SIGUSR1（nudge subcommand / herdr plugin events）＝即時 re-scan。
			// チャネル容量 1 なので待機中の連打は 1 回に潰れる（十分:
			// nudge は差分の権威ではなく「早く見ろ」の合図でしかない）。
		}
	}
}
