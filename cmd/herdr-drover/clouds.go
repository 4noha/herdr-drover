package main

// 複数クラウド fan-out 設定（端末ごとマルチ Google アカウント）。1 台の PC
// （cloud agent）が **複数の独立したクラウド**（別 Google アカウント＝別 GCP
// プロジェクト/別 relay/別 SA 鍵）へ同時にセッションを push し、各々の relay で
// トンネル/コマンドを受けるための設定リスト。クラウド側は一切改変不要
// （PC 側 agent のみの機能）。cm internal/config/clouds.go の drover 移植
// （パス ~/.herdr-drover/・Config フィールド名 Project/RelayURL/Credentials/PCID）。
//
// 優先順位:
//   - ~/.herdr-drover/clouds.json が存在し非空 → そのリストを使う
//     （primary = 先頭。Phase 3 のリモート pane 注入は primary のみ＝窓衝突回避）
//   - 無ければ従来どおり単一クラウド（env/config.json の GCP_PROJECT 等）
//     ＝既存単一クラウド構成は挙動完全不変（後方互換）

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Cloud は 1 つのクラウド接続先。SAKeyPath は **クライアント個別**の SA 鍵
// （プロセス global の GOOGLE_APPLICATION_CREDENTIALS とは別＝複数併存可＝
// state.NewWithCredentials の option.WithCredentialsFile で個別注入）。
type Cloud struct {
	Project   string `json:"project"`               // GCP プロジェクト ID
	RelayURL  string `json:"relay_url"`             // Cloud Run relay の wss:// URL
	SAKeyPath string `json:"sa_key_path,omitempty"` // この PC がこのクラウドへ書く SA 鍵（空=ADC）
	PCName    string `json:"pc_name,omitempty"`     // クラウド上の端末名（空=cfg.PCID）
}

// fileExists は path が存在するか（enroll の「クラウド追加」判定用）。
func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// cloudsHaveProject は cs に project のクラウドが含まれるか（enroll の同一
// クラウド再 enroll 判定用＝同 project は追加でなく更新）。
func cloudsHaveProject(cs []Cloud, project string) bool {
	for _, cl := range cs {
		if cl.Project == project {
			return true
		}
	}
	return false
}

// cloudsFilePath は clouds.json のパス（~/.herdr-drover/clouds.json）。
func cloudsFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".herdr-drover", "clouds.json")
}

// LoadClouds は接続先クラウドのリストを返す。clouds.json があればそれ、無ければ
// env/config.json の単一クラウド（Project 未設定なら空）。各エントリの PCName
// 既定は cfg.PCID、SAKeyPath 既定は cfg.Credentials（単一クラウド後方互換）。
func (c Config) LoadClouds() []Cloud {
	if f := cloudsFilePath(); f != "" {
		if b, err := os.ReadFile(f); err == nil {
			var cs []Cloud
			if json.Unmarshal(b, &cs) == nil {
				out := cs[:0]
				for _, cl := range cs {
					if cl.Project == "" || cl.RelayURL == "" {
						continue // 不完全エントリは無視
					}
					if cl.PCName == "" {
						cl.PCName = c.PCID
					}
					out = append(out, cl)
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}
	// 後方互換: env/config.json の単一クラウド。
	if c.Project == "" {
		return nil
	}
	return []Cloud{{
		Project:   c.Project,
		RelayURL:  c.RelayURL,
		SAKeyPath: c.defaultSAKeyPath(),
		PCName:    c.PCID,
	}}
}

// defaultSAKeyPath は単一クラウドの SA 鍵パス。cfg.Credentials（env or
// config.json で解決済）があればそれ、無ければ既定 ~/.herdr-drover/sa.json
// （存在時）。enroll は対話シェルで env 無しのことが多く、その時に既存クラウドの
// 鍵参照を空で seed すると 2 つ目追加で 1 つ目が ADC 失敗で繋がらなくなる事故を防ぐ。
func (c Config) defaultSAKeyPath() string {
	if c.Credentials != "" {
		return c.Credentials
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	cand := filepath.Join(home, ".herdr-drover", "sa.json")
	if _, e := os.Stat(cand); e == nil {
		return cand
	}
	return ""
}

// AppendCloud は clouds.json にクラウドを追記する（project で dedupe＝同 project
// は上書き更新）。clouds.json が無ければ existing（通常は env 単一クラウド）を
// seed してから追記＝enroll で 2 つ目以降を足しても既存クラウドが消えない。
// 書込は tmp→rename で原子的（0600）。
func (c Config) AppendCloud(add Cloud, existing []Cloud) error {
	f := cloudsFilePath()
	if f == "" {
		return os.ErrInvalid
	}
	var list []Cloud
	if b, err := os.ReadFile(f); err == nil {
		_ = json.Unmarshal(b, &list)
	}
	if len(list) == 0 {
		list = append(list, existing...) // 初回: env 単一クラウドを seed
	}
	merged := list[:0]
	replaced := false
	for _, cl := range list {
		if cl.Project == add.Project {
			merged = append(merged, add) // 同 project は更新
			replaced = true
			continue
		}
		merged = append(merged, cl)
	}
	if !replaced {
		merged = append(merged, add)
	}
	b, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f)
}
