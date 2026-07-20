package main

// enroll — Web「＋ 端末を追加」で発行された一回限りコードを relay と交換し、
// SA 鍵と設定を ~/.herdr-drover/ に自動配置する（cm runCloudEnroll の HTTP
// 契約そのまま＝relay/web/Firestore は cm 資産を無改変共用）:
//
//	POST <httpBase>/enroll  form: code=<code>
//	  200 → {"gcp_project": ..., "relay_url": ..., "sa_json": ...}
//	  401 → コード無効/期限切れ（relay 側 ConsumePairing が一回消費・15m TTL）
//
// httpBase は --relay の wss://→https://（ws://→http://）変換（cm
// main.go:428-429 と同じ 1 回置換）。Web の command 文言は
// `claude-master cloud enroll <code> --relay wss://…` 固定（cm 資産＝改変
// 禁止）なので、ユーザーは code と --relay を本コマンドへ読み替える:
//
//	herdr-drover enroll <code> --relay wss://<host>
//
// cm との差分: 配置先は ~/.herdr-drover/（sa.json＋config.json）。drover は
// 複数クラウド fan-out 未設計（DESIGN）なので cm の clouds.json 追記経路は
// 写さず、初回単一クラウド経路のみ（再 enroll は同ファイル上書き）。
// PC id は必ず <host>-herdr（DESIGN 決定事項）＝失効解除もその id で行う。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/4noha/drover-cloud/state"
)

// errUsage は使い方エラー（exit 2）を実行時エラー（exit 1）と区別する
// sentinel（run() の dispatch が判定する）。
var errUsage = errors.New("usage")

func cmdEnroll(args []string, stdout io.Writer) error {
	var code, relay string
	var slave bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--relay":
			if i+1 < len(args) {
				relay = args[i+1]
				i++
			}
		case "--slave":
			slave = true
		default:
			if code == "" {
				code = args[i]
			}
		}
	}
	if code == "" || relay == "" {
		return fmt.Errorf("%w: herdr-drover enroll <code> --relay wss://<host> [--slave]（code は Web「＋ 端末を追加」で発行。表示コマンドは claude-master 用なので読み替える。--slave は共用 PC＝SA レス）", errUsage)
	}

	// wss→https / ws→http（cm と同じ各 1 回置換）。
	httpBase := strings.Replace(strings.Replace(relay,
		"wss://", "https://", 1), "ws://", "http://", 1)
	// DefaultClient は Timeout ゼロ＝relay がブラックホール（SYN drop 等）だと
	// 無期限ハングする（レビュー指摘）。selfupdate と同流儀で明示 Timeout。
	hc := &http.Client{Timeout: 30 * time.Second}
	// master POST は `code=<code>` のみ＝byte 同一。slave のみ pc/role を足す
	// （relay が slaves/{pc} を bind するのに pc 名が要る）。slave の pc 名は
	// resolveConfig 解決値（env PC_ID > file > <host>-herdr）。
	form := url.Values{"code": {code}}
	var slaveCfg Config
	if slave {
		slaveCfg, _ = resolveConfig()
		form.Set("pc", slaveCfg.PCID)
		form.Set("role", "slave")
	}
	resp, err := hc.PostForm(httpBase+"/enroll", form)
	if err != nil {
		return fmt.Errorf("enroll 接続失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// 「コード無効/期限切れ」は 401 のみ。404/5xx まで同文言にすると
		// --relay typo や relay 障害を誤診させる（cm「403 だけでは区別
		// 不能」教訓と同型）。body 先頭を添えて一次情報を残す。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		if resp.StatusCode == 401 {
			return fmt.Errorf("enroll 失敗(401): コードが無効か期限切れ（一回限り・15 分）")
		}
		return fmt.Errorf("enroll 失敗(%d): relay/URL 側の問題の可能性（--relay を確認）: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var b struct {
		GCPProject  string `json:"gcp_project"`
		RelayURL    string `json:"relay_url"`
		SAJSON      string `json:"sa_json"`
		SlaveSecret string `json:"slave_secret"` // slave enroll のみ（master 応答には無い＝空）
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return fmt.Errorf("enroll 応答解析失敗: %w", err)
	}
	if b.GCPProject == "" {
		return fmt.Errorf("enroll 応答に gcp_project が無い（relay 側の GCP_PROJECT 未設定の疑い）")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home ディレクトリ不明: %w", err)
	}
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s 作成失敗: %w", dir, err)
	}
	relayURL := b.RelayURL
	if relayURL == "" {
		relayURL = relay // 応答に無ければ --relay 引数へ fallback（cm 同順）
	}

	// slave（共用 PC）は SA レス経路へ分岐する。ここまでの decode/home 解決は
	// master と共通で、以降の master 用 additional/config 書込ブロックには
	// 一切触れない（byte 同一のため verbatim 保持）。
	if slave {
		return cmdEnrollSlave(b.GCPProject, relayURL, b.SlaveSecret, dir, slaveCfg, stdout)
	}

	// クラウド追加判定（端末ごとマルチ Google アカウント fan-out）: clouds.json
	// が既にある(複数クラウド運用)か、既存の単一クラウドと**別プロジェクト**を
	// enroll する場合は clouds.json へ追記する。それ以外(初回 or 同一クラウド
	// 再 enroll)は従来どおり sa.json+config.json＝既存単一クラウド構成は挙動
	// 完全不変（後方互換）。pc id は resolveConfig 解決値（env PC_ID > file >
	// <host>-herdr）。
	rcfg, _ := resolveConfig()
	existing := rcfg.LoadClouds()
	additional := fileExists(cloudsFilePath()) ||
		(len(existing) > 0 && !cloudsHaveProject(existing, b.GCPProject))

	saPath := ""
	if b.SAJSON != "" {
		if additional {
			// 既存 sa.json を上書きしない per-project 鍵（GCP project ID は
			// 小文字/数字/ハイフンのみ＝ファイル名安全）。
			saPath = filepath.Join(dir, "sa-"+b.GCPProject+".json")
		} else {
			saPath = filepath.Join(dir, "sa.json")
		}
		// writeFileAtomic は CreateTemp(0600)→chmod→rename＝内容も権限も原子的
		//（os.WriteFile の truncate 直書き 0B 窓＋緩い権限残存バグの回避）。
		if err := writeFileAtomic(saPath, []byte(b.SAJSON), 0o600); err != nil {
			return fmt.Errorf("SA 鍵書込失敗: %w", err)
		}
	}

	cfgPath, err := configFilePath()
	if err != nil {
		return err
	}
	if additional {
		// 2 つ目以降: clouds.json へ追記（既存クラウドは seed で保持・同 project
		// は更新）。config.json（primary）は不変＝agent は次回起動で全クラウドへ
		// fan-out する（state.NewWithCredentials で SA を個別注入）。
		nc := Cloud{Project: b.GCPProject, RelayURL: relayURL, SAKeyPath: saPath, PCName: rcfg.PCID}
		if err := rcfg.AppendCloud(nc, existing); err != nil {
			return fmt.Errorf("clouds.json 書込失敗: %w", err)
		}
	} else {
		// 初回(単一クラウド): config.json の 3 キーのみ置換し他キー（pc_id 等の
		// 手動設定・learn_moves 等の別経路トグル）は保持する（生 JSON map で
		// 読み書き＝fileConfig へ decode→全置換すると未知キーが silent drop され、
		// 再 enroll のたび learn_moves が無警告で消える実バグの回避）。
		raw, ferr := readRawFileConfig(cfgPath)
		if ferr != nil {
			// 壊れた既存ファイルは enroll で作り直す（黙って上書きせずログに残す）。
			fmt.Fprintf(stdout, "⚠ 既存の設定ファイルが壊れているため作り直します: %v\n", ferr)
			raw = map[string]json.RawMessage{}
		}
		for k, v := range map[string]string{
			"gcp_project":                    b.GCPProject,
			"cloud_relay_url":                relayURL,
			"google_application_credentials": saPath,
		} {
			if v == "" {
				delete(raw, k) // 空値はキー削除（fileConfig omitempty と同じ見た目）
				continue
			}
			j, merr := json.Marshal(v)
			if merr != nil {
				return fmt.Errorf("設定 encode 失敗: %w", merr)
			}
			raw[k] = j
		}
		// learn_moves は初期状態で true をデフォルトに（新規セットアップは即 live 学習）。
		// 既存キーは尊重＝ユーザーが明示 false にした設定を上書きしない（enroll は 3
		// キーのみ置換の規律に沿う）。
		seedLearnMovesDefault(raw)
		if err := writeRawFileConfig(cfgPath, raw); err != nil {
			return fmt.Errorf("設定書込失敗: %w", err)
		}
	}

	// owner 発行コードでの正規 enroll＝再認可。過去の強制失効（Web「端末
	// ペアリング解除」）を best-effort で解除する（cm 同順・10s timeout。
	// 信頼の起点は owner 発行の一回限りコード）。rcfg は上で解決済。
	if saPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if est, e := state.NewWithCredentials(ctx, b.GCPProject, rcfg.PCID, saPath); e == nil {
			_ = est.ClearRevoked(ctx, rcfg.PCID)
			est.Close()
		}
		cancel()
	}

	fmt.Fprintln(stdout, "端末を登録しました（このアカウントに参加）。")
	fmt.Fprintf(stdout, "  gcp_project     = %s\n  cloud_relay_url = %s\n", b.GCPProject, relayURL)
	if saPath != "" {
		fmt.Fprintf(stdout, "  認証鍵          = %s\n", saPath)
	}
	if additional {
		fmt.Fprintf(stdout, "  接続先追加      = %s（端末ごとマルチ Google アカウント fan-out）\n", cloudsFilePath())
	} else {
		fmt.Fprintf(stdout, "  設定ファイル    = %s（env が常に優先）\n", cfgPath)
	}
	fmt.Fprintf(stdout, "  pc id           = %s\n", rcfg.PCID)
	// 案内は install（launchd 常駐の正規手順）へ向ける。install は
	// config.json も解決するので、この enroll 直後に env なしで成立する
	// （旧: install が KEY=VALUE の config しか読まず案内どおりに進むと
	// GCP_PROJECT 未解決で必ず失敗した実バグの再発防止）。
	fmt.Fprintln(stdout, "次: `herdr-drover install` で launchd 常駐を登録（label "+launchdLabel+"・設定は config.json から自動解決）。手動起動は `herdr-drover agent`。herdr のセッションが Web 端末一覧に出ます。")
	return nil
}

// cmdEnrollSlave は共用 PC（slave）の enroll 後処理: SA レスの config.json＋
// durable refresh secret（slave.json）を配置する（DESIGN_SLAVE_SPEC §5.1）。
// master の additional/clouds.json 経路は一切通らない（単一クラウド固定）。
//
//   - config.json: 同名キーのみ置換で未知キー（learn_moves 等）を保持しつつ
//     gcp_project / cloud_relay_url を設定・role="slave" を注入・
//     google_application_credentials を削除（＝SA レス）。sa_json は完全無視
//     （relay が誤って返しても書かない防御）。
//   - slave.json（0600・原子）: relay 発行の refresh secret を保存。1h bearer
//     を /slave/token でこの secret から取得する（SA 鍵は一切持たない）。
//   - 残骸掃除: master→slave 再 enroll で古い sa.json / clouds.json が
//     defaultSAKeyPath に拾われる事故を防ぐため best-effort で削除する。
//   - ClearRevoked（失効解除）は行わない（SA 鍵が無く Firestore 直結不能。
//     失効解除は relay 側で owner が行う設計＝§3.4）。
func cmdEnrollSlave(gcpProject, relayURL, slaveSecret, dir string, cfg Config, stdout io.Writer) error {
	if slaveSecret == "" {
		return fmt.Errorf("enroll(slave) 応答に slave_secret が無い（relay が --slave 非対応か role 未指定の疑い）")
	}
	cfgPath, err := configFilePath()
	if err != nil {
		return err
	}
	// config.json は生 JSON map で読み書き（fileConfig へ decode すると未知キーが
	// silent drop される＝enroll.go master 側と同じ規律）。
	raw, ferr := readRawFileConfig(cfgPath)
	if ferr != nil {
		fmt.Fprintf(stdout, "⚠ 既存の設定ファイルが壊れているため作り直します: %v\n", ferr)
		raw = map[string]json.RawMessage{}
	}
	setStr := func(k, v string) error {
		if v == "" {
			delete(raw, k)
			return nil
		}
		j, merr := json.Marshal(v)
		if merr != nil {
			return fmt.Errorf("設定 encode 失敗: %w", merr)
		}
		raw[k] = j
		return nil
	}
	if err := setStr("gcp_project", gcpProject); err != nil {
		return err
	}
	if err := setStr("cloud_relay_url", relayURL); err != nil {
		return err
	}
	if err := setStr("role", "slave"); err != nil {
		return err
	}
	delete(raw, "google_application_credentials") // SA レス（空値=削除規則と同義）
	// learn_moves は初期状態で true をデフォルトに（master と同一規律）。既存キーは
	// 尊重（明示 false を上書きしない）。
	seedLearnMovesDefault(raw)
	if err := writeRawFileConfig(cfgPath, raw); err != nil {
		return fmt.Errorf("設定書込失敗: %w", err)
	}

	// slave.json（0600・原子書込）: durable refresh secret。
	sf := slaveFile{
		PC:            cfg.PCID,
		RefreshSecret: slaveSecret,
		RelayURL:      relayURL,
		GCPProject:    gcpProject,
	}
	sb, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return fmt.Errorf("slave.json encode 失敗: %w", err)
	}
	slavePath := filepath.Join(dir, "slave.json")
	if err := writeFileAtomic(slavePath, sb, 0o600); err != nil {
		return fmt.Errorf("slave.json 書込失敗: %w", err)
	}

	// 真に SA レスにする（残骸掃除・best-effort）。
	_ = os.Remove(filepath.Join(dir, "sa.json"))
	_ = os.Remove(filepath.Join(dir, "clouds.json"))

	fmt.Fprintln(stdout, "共用 PC（slave）として登録しました。SA 鍵は配布されません。")
	fmt.Fprintf(stdout, "  gcp_project     = %s\n  cloud_relay_url = %s\n", gcpProject, relayURL)
	fmt.Fprintln(stdout, "  role            = slave")
	fmt.Fprintf(stdout, "  設定ファイル    = %s（env が常に優先）\n", cfgPath)
	fmt.Fprintf(stdout, "  slave 認証情報  = %s（refresh secret・0600）\n", slavePath)
	fmt.Fprintf(stdout, "  pc id           = %s\n", cfg.PCID)
	fmt.Fprintln(stdout, "  注意: この共用 PC はオーナーの他 PC のセッションを見られません（自分の herdr セッションだけを共有＝私物漏れ防止）。")
	fmt.Fprintln(stdout, "次: `herdr-drover install` で launchd 常駐を登録（label "+launchdLabel+"）。手動起動は `herdr-drover agent`。")
	return nil
}

// seedLearnMovesDefault は config.json の生 JSON map に learn_moves=true を
// **キーが不在のときのみ** 書き込む（明示的に false を書いたユーザーの意思を
// 尊重）。新規 enroll の既定を true にしつつ、opt-out した設定は保持する。
func seedLearnMovesDefault(raw map[string]json.RawMessage) {
	if _, ok := raw["learn_moves"]; ok {
		return // 既存尊重（true/false どちらでも触らない）
	}
	raw["learn_moves"] = json.RawMessage("true")
}
