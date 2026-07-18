package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// defaultTick は producer の周期 poll backstop（DESIGN: events nudge＋周期
// poll 常設）。events は差分の権威ではない（herdrapi.Subscribe の実測:
// バックログ再送あり）ので、この周期が同期の正しさの下限を決める。
const defaultTick = 5 * time.Second

// Config は agent/status が使う実行設定。全て環境変数から解決する
// （cm と同じく launchd plist の EnvironmentVariables で与える運用）。
type Config struct {
	Project     string        // GCP_PROJECT（agent 必須）
	RelayURL    string        // CLOUD_RELAY_URL（Phase 2 Web ターミナル以降で必須）
	Credentials string        // GOOGLE_APPLICATION_CREDENTIALS（未設定なら ADC/エミュレータ）
	PCID        string        // PC_ID（既定 <hostname 短縮小文字>-herdr）
	SocketPath  string        // HERDR_SOCKET_PATH（解決は herdrapi.ResolveSocketPath と同一規則）
	Tick        time.Duration // DROVER_TICK（既定 5s）
	Idle        time.Duration // DROVER_IDLE（Web ターミナル quiescence 自切断。0=bridge 既定 30s）
}

// resolveConfig は Config を解決する。優先順位はキー単位で
// **env > 設定ファイル（~/.herdr-drover/config.json）> 既定**。
// 設定ファイルは enroll が書く永続設定（launchd plist の
// EnvironmentVariables 手転記より単純な経路）で、対象キーは
// gcp_project / cloud_relay_url / google_application_credentials / pc_id
// のみ（DROVER_TICK 等の運用 knob は従来どおり env 専用）。
// エラー時も判明した分は埋めた Config を返す（status が「壊れた設定でも
// 残りを表示」できるように）。
func resolveConfig() (Config, error) {
	cfg := Config{
		Project:     os.Getenv("GCP_PROJECT"),
		RelayURL:    os.Getenv("CLOUD_RELAY_URL"),
		Credentials: os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"),
		PCID:        os.Getenv("PC_ID"),
		SocketPath:  herdrapi.ResolveSocketPath(""),
		Tick:        defaultTick,
	}
	// 設定ファイル: env に無いキーだけ補完（env > file）。壊れたファイルは
	// エラーとして返すが解決は続行する（沈黙で無視すると enroll 済のはずが
	// 未設定で動く事故になる。契約: 判明分は埋めて返す）。
	var fileErr error
	if path, perr := configFilePath(); perr == nil {
		fc, ferr := readFileConfig(path)
		if ferr != nil {
			fileErr = ferr
		}
		if cfg.Project == "" {
			cfg.Project = fc.GCPProject
		}
		if cfg.RelayURL == "" {
			cfg.RelayURL = fc.CloudRelayURL
		}
		if cfg.Credentials == "" {
			cfg.Credentials = fc.GoogleApplicationCredentials
		}
		if cfg.PCID == "" {
			cfg.PCID = fc.PCID
		}
	}
	if cfg.PCID == "" {
		host, err := os.Hostname()
		if err != nil {
			return cfg, fmt.Errorf("PC_ID 未設定かつ hostname 取得失敗: %w", err)
		}
		cfg.PCID = defaultPCID(host)
	}
	if v := os.Getenv("DROVER_TICK"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("DROVER_TICK が不正（Go duration 形式。例 5s / 750ms）: %q: %w", v, err)
		}
		if d <= 0 {
			return cfg, fmt.Errorf("DROVER_TICK は正の期間であること: %q", v)
		}
		cfg.Tick = d
	}
	// DROVER_IDLE は Web ターミナルの quiescence 自切断（無通信でデータ線を
	// 自分から閉じる＝near-$0 の要）。未設定=0 は bridge 既定 30s（cm 本番
	// IdleClose と同値）。負値（quiescence 無効）は env からは許さない:
	// 常時接続化は near-$0 設計を壊すため、無効化はテストが bridge.Idle を
	// 直接触る時だけの内部 knob とする。
	if v := os.Getenv("DROVER_IDLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("DROVER_IDLE が不正（Go duration 形式。例 30s）: %q: %w", v, err)
		}
		if d <= 0 {
			return cfg, fmt.Errorf("DROVER_IDLE は正の期間であること（quiescence 無効化は不可＝near-$0 設計）: %q", v)
		}
		cfg.Idle = d
	}
	return cfg, fileErr
}

// configFilePath は ~/.herdr-drover/config.json（enroll が書く永続設定）。
func configFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home ディレクトリ不明（設定ファイル置き場を決められない）: %w", err)
	}
	return filepath.Join(home, ".herdr-drover", "config.json"), nil
}

// fileConfig は設定ファイルのスキーマ（キー名は env 名の小文字 snake_case
// ＝対応が一目で分かる exact-match。未知キーは無視される）。
type fileConfig struct {
	GCPProject                   string `json:"gcp_project,omitempty"`
	CloudRelayURL                string `json:"cloud_relay_url,omitempty"`
	GoogleApplicationCredentials string `json:"google_application_credentials,omitempty"`
	PCID                         string `json:"pc_id,omitempty"`
}

// readFileConfig は設定ファイルを読む。不在はゼロ値＋nil（enroll 前の
// 素の env 運用と完全同挙動＝後方互換）。壊れた JSON はエラー。
func readFileConfig(path string) (fileConfig, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileConfig{}, nil
	}
	if err != nil {
		return fileConfig{}, err
	}
	var fc fileConfig
	if err := json.Unmarshal(b, &fc); err != nil {
		return fileConfig{}, fmt.Errorf("設定ファイル %s が壊れている（JSON 解析失敗）: %w", path, err)
	}
	return fc, nil
}

// readRawFileConfig は設定ファイルを「キー→生 JSON 値」で読む（enroll の
// 「同名キーのみ置換」用。fileConfig と違い未知キー〔learn_moves 等の別経路
// トグル〕を落とさない）。不在は空 map＋nil。壊れた JSON はエラー。
func readRawFileConfig(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("設定ファイル %s が壊れている（JSON 解析失敗）: %w", path, err)
	}
	return raw, nil
}

// writeRawFileConfig は raw map（キー辞書順で整形）を原子的に書く。
func writeRawFileConfig(path string, raw map[string]json.RawMessage) error {
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return writeConfigBytesAtomic(path, b)
}

// writeConfigBytesAtomic は設定ファイルを tmp→rename で原子的に書く（cm
// WriteStatus 教訓: truncate 直書きは 0B の瞬間が実観測される）。0600＝
// SA 鍵パス等を含むため所有者のみ（CreateTemp の既定 perm）。
func writeConfigBytesAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp") // 0600 で作られる
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// defaultPCID は hostname から既定 pc id を作る。
// 短縮（最初のドットまで）＋小文字化＋"-herdr" 固定サフィックス。
//
// DESIGN 決定事項: pc id は必ず <host>-herdr＝cm agent と分離する。同一 id
// を使うと双方の producer が相手のセッションを「消滅キー」と誤認して
// DeleteSession し合う削除合戦になる（検証済の実挙動）。
func defaultPCID(hostname string) string {
	short, _, _ := strings.Cut(hostname, ".")
	return strings.ToLower(short) + "-herdr"
}

// warnConfig は agent 起動時に出す非致命の設定警告（純関数＝機械検証可能）。
// エラーにしない理由も各警告文に書く。
func warnConfig(cfg Config) []string {
	var ws []string
	if !strings.HasSuffix(cfg.PCID, "-herdr") {
		// 明示 PC_ID の上書きは運用の自由として許すが、cm agent と同一 id に
		// した場合の削除合戦（上記 defaultPCID コメント）は重大なので警告する。
		ws = append(ws, fmt.Sprintf("⚠ PC_ID=%q が -herdr サフィックスを持たない。cm agent と同一 id にすると DeleteSession の削除合戦になる（DESIGN 決定事項）", cfg.PCID))
	}
	if cfg.RelayURL == "" {
		ws = append(ws, "⚠ CLOUD_RELAY_URL 未設定。Phase 1（一覧同期）は動くが Web ターミナル（Phase 2）は不可")
	}
	if cfg.Credentials == "" && os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		// gcloud auth application-default 等の ADC で動く構成もあるので
		// エラーにはしない（Firestore 接続失敗なら state.New が実エラーを返す）。
		ws = append(ws, "⚠ GOOGLE_APPLICATION_CREDENTIALS 未設定（ADC に依存。SA 鍵運用なら設定を推奨）")
	}
	return ws
}
