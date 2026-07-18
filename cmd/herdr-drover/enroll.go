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

	"github.com/4noha/herdr-drover/internal/cloud/state"
)

// errUsage は使い方エラー（exit 2）を実行時エラー（exit 1）と区別する
// sentinel（run() の dispatch が判定する）。
var errUsage = errors.New("usage")

func cmdEnroll(args []string, stdout io.Writer) error {
	var code, relay string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--relay":
			if i+1 < len(args) {
				relay = args[i+1]
				i++
			}
		default:
			if code == "" {
				code = args[i]
			}
		}
	}
	if code == "" || relay == "" {
		return fmt.Errorf("%w: herdr-drover enroll <code> --relay wss://<host>（code は Web「＋ 端末を追加」で発行。表示コマンドは claude-master 用なので読み替える）", errUsage)
	}

	// wss→https / ws→http（cm と同じ各 1 回置換）。
	httpBase := strings.Replace(strings.Replace(relay,
		"wss://", "https://", 1), "ws://", "http://", 1)
	// DefaultClient は Timeout ゼロ＝relay がブラックホール（SYN drop 等）だと
	// 無期限ハングする（レビュー指摘）。selfupdate と同流儀で明示 Timeout。
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.PostForm(httpBase+"/enroll", url.Values{"code": {code}})
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
		GCPProject string `json:"gcp_project"`
		RelayURL   string `json:"relay_url"`
		SAJSON     string `json:"sa_json"`
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
	saPath := ""
	if b.SAJSON != "" {
		saPath = filepath.Join(dir, "sa.json")
		// os.WriteFile は truncate 直書き（0B の瞬間）＋既存ファイルの緩い
		// 権限（0644 等）が残る実測バグ（レビュー指摘）。writeFileAtomic は
		// CreateTemp(0600)→chmod→rename＝内容も権限も原子的に置換する。
		if err := writeFileAtomic(saPath, []byte(b.SAJSON), 0o600); err != nil {
			return fmt.Errorf("SA 鍵書込失敗: %w", err)
		}
	}

	// 設定ファイル: 既存の他キー（pc_id 等の手動設定、learn_moves 等の
	// fileConfig 4 キー外の別経路トグルも含む）は保持して 3 キーのみ置換する
	// （cm writeTomlKeys の「同名キーのみ置換」と同じ規律）。生 JSON map で
	// 読み書きする理由: fileConfig へ decode→全置換すると未知キーが silent に
	// drop され、再 enroll のたび learn_moves が無警告で消える実バグだった
	// （レビュー指摘＝readLearnMoves の「silent に無効化しない」規律の裏口破り）。
	cfgPath, err := configFilePath()
	if err != nil {
		return err
	}
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
	if err := writeRawFileConfig(cfgPath, raw); err != nil {
		return fmt.Errorf("設定書込失敗: %w", err)
	}

	// owner 発行コードでの正規 enroll＝再認可。過去の強制失効（Web「端末
	// ペアリング解除」）を best-effort で解除する（cm 同順・10s timeout。
	// 信頼の起点は owner 発行の一回限りコード）。pc id は resolveConfig の
	// 解決値＝env PC_ID > file > <host>-herdr（DESIGN: -herdr 固定）。
	rcfg, _ := resolveConfig()
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
	fmt.Fprintf(stdout, "  設定ファイル    = %s（env が常に優先）\n", cfgPath)
	fmt.Fprintf(stdout, "  pc id           = %s\n", rcfg.PCID)
	// 案内は install（launchd 常駐の正規手順）へ向ける。install は
	// config.json も解決するので、この enroll 直後に env なしで成立する
	// （旧: install が KEY=VALUE の config しか読まず案内どおりに進むと
	// GCP_PROJECT 未解決で必ず失敗した実バグの再発防止）。
	fmt.Fprintln(stdout, "次: `herdr-drover install` で launchd 常駐を登録（label "+launchdLabel+"・設定は config.json から自動解決）。手動起動は `herdr-drover agent`。herdr のセッションが Web 端末一覧に出ます。")
	return nil
}
