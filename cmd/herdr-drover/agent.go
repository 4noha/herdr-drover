package main

// agent — 常駐 daemon（launchd から起動。ProcessType=Background は禁止＝
// cm STATUS flap 事故の教訓: throttle された scan が timeout → 空同期の
// 正帰還になる）。
//
// 役割: 設定解決 → pidfile → herdr ping → 接続先クラウド解決（clouds.json＝
// マルチ Google アカウント fan-out／無ければ env 単一クラウド）→ クラウドごと
// に RegisterPC → producer ループ（周期 tick＋SIGUSR1 即時 re-scan）→ webterm
// 制御線 → 遠隔命令 → SIGTERM/SIGINT で graceful 終了。

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/4noha/drover-cloud/selfupdate"
	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/commands"
	"github.com/4noha/herdr-drover/internal/herdrapi"
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
	// 接続先クラウド（clouds.json＝マルチ Google アカウント fan-out、無ければ
	// env/config.json の単一クラウド＝後方互換）。空は Firestore 同期不能＝致命。
	clouds := cfg.LoadClouds()
	if len(clouds) == 0 {
		return fmt.Errorf("接続先クラウドが無い（GCP_PROJECT 未設定 or ~/.herdr-drover/clouds.json が空）")
	}
	for _, w := range warnConfig(cfg) {
		lg.Println(w)
	}

	// 二重起動ゲート（プロセス単位＝全クラウドで共有）: flock をプロセス生存
	// 期間中保持する（check-then-write の TOCTOU 排除。詳細は pidfile.go）。
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

	// herdr ping（プロセス単位で 1 回＝全クラウドで同じ hcli を共有）。接続
	// 不能は即エラー終了（launchd KeepAlive が再起動）。version/protocol は
	// 起動ログに必ず残す＝障害調査の一次情報。
	hcli := herdrapi.New(cfg.SocketPath)
	pong, err := hcli.Ping()
	if err != nil {
		return fmt.Errorf("herdr へ接続できない（socket=%s）: %w", cfg.SocketPath, err)
	}
	lg.Printf("herdr ok: version=%s protocol=%d (socket=%s)", pong.Version, pong.Protocol, cfg.SocketPath)
	for _, w := range verifyHerdr(pong) {
		lg.Println(w)
	}

	// graceful 終了（SIGTERM/SIGINT）と nudge（SIGUSR1）は別チャネル: 前者は
	// ctx cancel、後者は producer ループの即時 re-scan トリガ。
	// 順序メモ（レビュー指摘の検証結果＝棄却）: Go runtime は Notify 未登録の
	// SIGUSR1 を黙って捨てる＝pidfile 書込〜Notify の窓で受けても死なない
	// （sigtable SIGUSR1=_SigNotify のみ・実 agent で実測）。窓で失われた
	// nudge は runAgentLoop の起動直後 1 tick が取りこぼしを埋める。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	nudge := make(chan os.Signal, 1)
	signal.Notify(nudge, syscall.SIGUSR1)
	defer signal.Stop(nudge)

	// live 学習（opt-in・既定 false＝挙動完全不変）: config.json の
	// learn_moves=true のときだけ pane.moved を購読し手動 Tab 移動を wsmap の
	// exact ルールへ自動反映（organize.go runLearnLoop）。**プロセス単位で 1 回**
	// ＝wsmap はローカル・クラウド非依存（クラウドごとに購読すると多重学習・
	// 競合になる）。バックログ再送の誤学習はライブ状態照合で dedup。
	if learnOn, lerr := readLearnMoves(); lerr != nil {
		lg.Printf("learn: 設定読取エラー（live 学習は無効で継続）: %v", lerr)
	} else if learnOn {
		lg.Printf("learn: live 学習有効（learn_moves=true・pane.moved 購読）")
		go func() {
			if err := runLearnLoop(ctx, hcli, lg); err != nil && ctx.Err() == nil {
				lg.Printf("learn: loop 終了: %v", err)
			}
		}()
	}
	lg.Printf("agent 開始: pc=%s clouds=%d tick=%s version=%s", cfg.PCID, len(clouds), cfg.Tick, version)

	// 単一クラウド（env or clouds.json 1 件）: 従来どおり inline＝fail-fast
	// （初期化エラーを return して露出・launchd が再起動）＝挙動完全不変。
	if len(clouds) == 1 {
		return runOneCloud(ctx, cfg, clouds[0], true, hcli, nudge, "", lg)
	}

	// 複数クラウド: クラウドごと goroutine。1 クラウドの初期化失敗は log して
	// 他を止めない（fail-fast でなく log-and-continue＝1 アカウントの不調が
	// 他アカウントの同期を巻き込まない）。SIGUSR1 nudge は各クラウドの
	// producer ループへ配る（channel は 1 goroutine しか読めないため）。
	subNudges := make([]chan os.Signal, len(clouds))
	for i := range subNudges {
		subNudges[i] = make(chan os.Signal, 1)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s := <-nudge:
				for _, sn := range subNudges {
					select {
					case sn <- s:
					default: // 容量 1＝待機中の連打は 1 回に潰す
					}
				}
			}
		}
	}()
	var wg sync.WaitGroup
	for i, cl := range clouds {
		wg.Add(1)
		tag := fmt.Sprintf("[%s] ", cl.Project)
		go func(cl Cloud, primary bool, nd <-chan os.Signal, tag string) {
			defer wg.Done()
			if err := runOneCloud(ctx, cfg, cl, primary, hcli, nd, tag, lg); err != nil && ctx.Err() == nil {
				lg.Printf("%sクラウド接続終了（他クラウドは継続）: %v", tag, err)
			}
		}(cl, i == 0, subNudges[i], tag)
	}
	wg.Wait()
	lg.Printf("agent 終了（graceful・全クラウド停止）")
	return nil
}

// runOneCloud は 1 クラウドへの接続（RegisterPC → webterm 制御線 → 遠隔命令 →
// producer ループ）を回す。tag はログ接頭辞（単一クラウドは ""＝従来ログと
// 同一・複数は "[project] "）。primary（先頭クラウド）は将来のリモート pane
// 注入（Phase 3・DESIGN）を primary 限定にするための予約フラグ（現状 push/
// relay/command は全クラウドで動く＝primary は未使用）。ctx 終了で graceful
// 戻り。err 返却＝初期化失敗（呼び手が単一なら fail-fast、複数なら log 継続）。
func runOneCloud(ctx context.Context, cfg Config, cl Cloud, primary bool, hcli *herdrapi.Client, nudge <-chan os.Signal, tag string, lg *log.Logger) error {
	// 資格情報はクラウド個別に注入する（GOOGLE_APPLICATION_CREDENTIALS は
	// process global で 1 つ＝複数クラウド併存には option.WithCredentialsFile
	// が必須＝これが fan-out の肝）。エミュレータ時は資格情報を渡さない
	// （不要かつ client 生成オプションの非互換を踏まないため。e2e はこの経路）。
	creds := cl.SAKeyPath
	if os.Getenv("FIRESTORE_EMULATOR_HOST") != "" {
		creds = ""
	}
	st, err := state.NewWithCredentials(ctx, cl.Project, cl.PCName, creds)
	if err != nil {
		return fmt.Errorf("Firestore 接続失敗（project=%s）: %w", cl.Project, err)
	}
	defer st.Close()

	// 起動時 1 回の親 doc 登録（session ゼロの idle PC も Web 一覧に出す＋
	// agent 版可視化）。ただし強制失効済なら登録しない（owner の Web「端末
	// ペアリング解除」で消した端末が launchd 再起動で蘇る削除合戦を防ぐ。
	// cm runOneCloud と同規律・回帰 test/ TestE2ERevokedAgentDormant）。
	if st.IsSelfRevoked(ctx) {
		lg.Printf("%sこの端末（pc=%s）はペアリング解除済（dormant）。再 enroll / 失効解除で復帰します", tag, cl.PCName)
	} else if err := st.RegisterPCVersion(ctx, version); err != nil {
		return fmt.Errorf("RegisterPC 失敗（資格情報/プロジェクト設定を確認・project=%s）: %w", cl.Project, err)
	}
	lg.Printf("%sクラウド開始: pc=%s project=%s relay=%s", tag, cl.PCName, cl.Project, cl.RelayURL)

	// Phase 2 Web ターミナル: relay 設定時のみ制御線（WatchWake）を起動。
	// SIGTERM は ctx cancel → WatchWake/全 bridge 停止 → 下の drain で bounded 待ち。
	var wt *webTerm
	if cl.RelayURL != "" {
		wt = newWebTerm(cl.RelayURL, cfg.Idle, st, hcli, lg)
		wt.start(ctx)
		lg.Printf("%swebterm: WatchWake 起動（relay=%s）", tag, cl.RelayURL)
	}

	// Phase 3 リモート pane 注入（↗窓相当）: **primary（先頭）クラウドのみ**が
	// 他 PC のセッションをローカル herdr へ注入 pane として同期する（複数クラウドが
	// 同一 herdr へ同 pane を注入する二重窓・競合を構造的に防ぐ）。relay 必須
	// （注入 pane 内の attach viewer がリモート relay へ繋ぐ）。
	if primary && cl.RelayURL != "" {
		go runRemoteInject(ctx, hcli, st, cl.PCName, lg)
		lg.Printf("%sリモート pane 注入 起動（他 PC のセッションを↗注入・primary）", tag)
	}

	// 遠隔命令制御線。DoRestart/DoUpdate/DoExit はプロセス単位（どのクラウドの
	// コマンドでも自分自身＝プロセス全体を再起動/更新）。DoProxy は当該クラウドの
	// webterm（sid はそのクラウドの relay grant に紐づく）。破壊的命令は Ack 先行。
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
			lg.Printf("%scommand watcher 終了: %v", tag, err)
		}
	}()

	// producer 契約（internal/session と統合）: 1 tick = pane.list/agent.list →
	// session map（key=pane_id・short_dir=cwd 末尾・window_name=agent/label・
	// is_active=AgentStatus）→ st.PushStatus（content_hash ゲート＝near-$0）。
	// 消滅キーは前 tick 差分で st.DeleteSession（初回 prev は OwnSessionKeys で
	// seed）。scan エラー時は Push/Delete せず error を返す＝呼び手（runAgentLoop）
	// がその tick を skip＝前回状態維持（cm の空 STATUS flap 事故の教訓）。
	// 各クラウドが自分の producer を持つ＝同じ herdr セッションを各クラウドへ push。
	prod := session.NewProducer(hcli, st)

	// tick 冒頭の失効検査（tick 毎 1 read で near-$0 規律内）: 失効中は scan/
	// push/delete を一切せず doc を再作成しない。owner の ClearRevoked（再
	// enroll）で次 tick 自然復帰。ログは遷移時のみ（dormant 中に同文を tick
	// 毎に吐かない）。
	dormant := false
	tickFn := func(ctx context.Context) error {
		if st.IsSelfRevoked(ctx) {
			if !dormant {
				lg.Printf("%sペアリング解除を検出（dormant へ移行・push/delete 停止）", tag)
				dormant = true
			}
			return nil
		}
		if dormant {
			lg.Printf("%s失効解除を検出（同期再開）", tag)
			dormant = false
		}
		return prod.Tick(ctx)
	}

	runAgentLoop(ctx, cfg.Tick, nudge, tickFn, lg)
	if wt != nil {
		wt.drain(3 * time.Second)
	}
	lg.Printf("%sクラウド終了（graceful）", tag)
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
