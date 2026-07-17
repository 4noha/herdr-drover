package main

// status — daemon 生存・herdr 接続・設定の充足を人間可読で表示する。
// herdr plugin の action "status" からも呼ばれる（herdr-plugin.toml）。
//
// これは「報告」であって「probe」ではない: daemon 停止や herdr 未接続でも
// exit 0 で全項目を表示し切る（一部の失敗で残りの診断情報を隠さない）。
// exit != 0 は status 自身が表示を作れなかった時のみ。

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func cmdStatus(stdout io.Writer) error {
	fmt.Fprintf(stdout, "herdr-drover %s status\n", version)

	// resolveConfig はエラーでも判明分を埋めて返す契約＝表示は続行する。
	cfg, cfgErr := resolveConfig()

	// --- daemon ---
	if path, err := pidfilePath(); err != nil {
		fmt.Fprintf(stdout, "daemon : 不明（%v）\n", err)
	} else if pid, rerr := readPidfile(path); errors.Is(rerr, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "daemon : 停止中（pidfile %s なし）\n", path)
	} else if rerr != nil {
		fmt.Fprintf(stdout, "daemon : 不明（%v）\n", rerr)
	} else if pidAlive(pid) {
		fmt.Fprintf(stdout, "daemon : 稼働中（pid %d・pidfile %s）\n", pid, path)
	} else {
		fmt.Fprintf(stdout, "daemon : 停止中（stale pidfile %s: pid %d は不在）\n", path, pid)
	}

	// --- herdr ---
	// 一発コマンドなので dial 5s / call 30s の既定は長すぎる＝短縮。
	hcli := herdrapi.New(cfg.SocketPath)
	hcli.Timeout = 3 * time.Second
	if pong, err := hcli.Ping(); err != nil {
		fmt.Fprintf(stdout, "herdr  : NG（socket=%s）: %v\n", cfg.SocketPath, err)
	} else {
		fmt.Fprintf(stdout, "herdr  : OK version=%s protocol=%d（socket=%s）\n", pong.Version, pong.Protocol, cfg.SocketPath)
		for _, w := range verifyHerdr(pong) {
			fmt.Fprintf(stdout, "         %s\n", w)
		}
	}

	// --- config ---
	fmt.Fprintf(stdout, "config :\n")
	if cfgErr != nil {
		fmt.Fprintf(stdout, "  ⚠ 設定エラー: %v\n", cfgErr)
	}
	printKV := func(k, v, missing string) {
		if v == "" {
			fmt.Fprintf(stdout, "  %-30s = （未設定）%s\n", k, missing)
			return
		}
		fmt.Fprintf(stdout, "  %-30s = %s\n", k, v)
	}
	printKV("GCP_PROJECT", cfg.Project, " ⚠ agent 起動に必須")
	printKV("CLOUD_RELAY_URL", cfg.RelayURL, " ⚠ Web ターミナル（Phase 2）で必要")
	if cfg.Credentials != "" {
		if _, err := os.Stat(cfg.Credentials); err != nil {
			fmt.Fprintf(stdout, "  %-30s = %s ⚠ 読めない: %v\n", "GOOGLE_APPLICATION_CREDENTIALS", cfg.Credentials, err)
		} else {
			fmt.Fprintf(stdout, "  %-30s = %s（存在）\n", "GOOGLE_APPLICATION_CREDENTIALS", cfg.Credentials)
		}
	} else {
		printKV("GOOGLE_APPLICATION_CREDENTIALS", "", "（ADC / FIRESTORE_EMULATOR_HOST に依存）")
	}
	printKV("PC_ID", cfg.PCID, "")
	fmt.Fprintf(stdout, "  %-30s = %s\n", "DROVER_TICK", cfg.Tick)
	fmt.Fprintf(stdout, "  %-30s = %s\n", "HERDR_SOCKET_PATH(解決値)", cfg.SocketPath)
	return nil
}
