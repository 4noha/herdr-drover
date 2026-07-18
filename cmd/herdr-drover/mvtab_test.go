package main

// mv-tab の純関数テスト（対話ピッカの入力パース・非対話フラグ検証）。
// 実 Tab 移動は organize_test.go の moveWholeTab 実 herdr 隔離テストで担保済＝
// 本ファイルは対話 I/O 層と cmdMvTab のフラグ分岐のみ検証。

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestPromptChoiceValid(t *testing.T) {
	stdin := strings.NewReader("2\n")
	var stdout bytes.Buffer
	n, err := promptChoice(stdin, &stdout, "番号: ", 1, 5)
	if err != nil {
		t.Fatalf("promptChoice: %v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if !strings.Contains(stdout.String(), "番号: ") {
		t.Errorf("prompt が stdout に出ていない: %q", stdout.String())
	}
}

func TestPromptChoiceRange(t *testing.T) {
	cases := []struct {
		input   string
		wantErr string
	}{
		{"0\n", "範囲外"},
		{"6\n", "範囲外"},
		{"abc\n", "整数でない"},
		{"\n", "空入力"},
		{"", "入力読取"}, // EOF
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			stdin := strings.NewReader(c.input)
			var stdout bytes.Buffer
			_, err := promptChoice(stdin, &stdout, "n: ", 1, 5)
			if err == nil {
				t.Fatalf("エラーを期待した")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("エラーメッセージ不一致: got=%v want含む=%q", err, c.wantErr)
			}
		})
	}
}

func TestCmdMvTabRejectsBareInvocationOffTTY(t *testing.T) {
	// 引数なし＝対話ピッカ要求。stdin が TTY でなければ明示エラー（曖昧回避）。
	// stdinIsTTY は claudeshim.go の実 ioctl 経路＝本 test プロセスでは stdin は
	// pipe/devnull＝非 TTY のはず（go test 実行環境の実測）。
	var stdout, stderr bytes.Buffer
	err := cmdMvTab([]string{}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("非 TTY で引数なし cmdMvTab はエラーを返すべき")
	}
	if !strings.Contains(err.Error(), "TTY") {
		t.Errorf("エラーメッセージに TTY を含むべき: %v", err)
	}
}

func TestCmdMvTabAcceptsFullFlagsWithoutTTY(t *testing.T) {
	// --src-tab と --dst-ws を両方指定すれば非対話＝TTY 不要。
	// 実 herdr socket が無ければ tab.list で失敗するが、TTY エラーで拒否されない
	// ことだけを見る（実 herdr 依存の緑パスは organize_test.go の実 herdr で検証済）。
	var stdout, stderr bytes.Buffer
	err := cmdMvTab([]string{"--src-tab", "w1:t1", "--dst-ws", "w2"}, strings.NewReader(""), &stdout, &stderr)
	// エラーは api dial か tab.list の失敗であるべき（TTY エラーでない）。
	if err != nil && strings.Contains(err.Error(), "TTY") {
		t.Errorf("フル指定で TTY 拒否は誤り: %v", err)
	}
}

func TestCmdMvTabParseErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// 余分な位置引数は拒否。
	err := cmdMvTab([]string{"extra-positional"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatal("余分な位置引数はエラーを返すべき")
	}
	if !strings.Contains(err.Error(), "使い方") && !strings.Contains(err.Error(), "usage") {
		t.Errorf("エラーメッセージが使い方を示すべき: %v", err)
	}
}

func TestCmdMvTabLaunchNeedsHerdrOrEnv(t *testing.T) {
	// HERDR_WORKSPACE_ID を明示 unset・env なしでも api dial 失敗まで進む
	// （launcher 自体のロジックのフェイルセーフ確認）。
	t.Setenv("HERDR_WORKSPACE_ID", "")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/nonexistent-drover-test.sock")
	err := cmdMvTabLaunch(os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("herdr socket 不在ではエラーを返すべき")
	}
	// dial エラーが起きているはず（フォーカス WS 解決失敗の layer）。
	if !strings.Contains(err.Error(), "herdr") && !strings.Contains(err.Error(), "focus") && !strings.Contains(err.Error(), "socket") {
		t.Errorf("エラーメッセージが socket/herdr を示すべき: %v", err)
	}
}
