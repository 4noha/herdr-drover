package autocontinue

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
)

// transcriptTailBytes は transcript の末尾読取量。直近のユーザ指示を拾うだけ
// なので全読みしない（Claude Code の transcript は数十 MB になる）。
const transcriptTailBytes = 2 << 20

// ReasonPrefix は自動続行の reason に付ける接頭辞（画面で見分ける・transcript
// から自分の続行文を除外する目印）。
const ReasonPrefix = "[semi-auto"

var (
	// reGrant: 「いけるところまで」相当の裁量付与（実ログで見えた言い回し）。
	reGrant = regexp.MustCompile(`(いける|行ける|できる|出来る|可能な)(ところ|とこ|限り|範囲)まで|最後まで|完成まで|自律的に|できることを|全部やって|止まらず|聞かずに|as far as you can|until (it'?s )?done|autonomously`)
	// reContinueLike: 短い「続けろ」系の指示（裁量ありならそのまま文言に再利用）。
	reContinueLike = regexp.MustCompile(`(つづけ|続け|続き|つづき|続行|continue|go on|進めて|すすめて)`)
)

// GrantIn は指示文に裁量付与の言い回しがあるか。
func GrantIn(prompt string) bool { return reGrant.MatchString(prompt) }

// ContinueLike は短い「続けろ」系か（40 字以内＋語を含む）。
func ContinueLike(prompt string) bool {
	p := strings.TrimSpace(prompt)
	return p != "" && len([]rune(p)) <= 40 && reContinueLike.MatchString(p)
}

// LastHumanPrompt は transcript から直近の**人間の**入力テキストを返す。
// agent は claude / codex（それ以外は未対応＝false）。自動続行の文
// （ReasonPrefix / "Stop hook feedback:"）・tool_result・`<` 始まりの環境注入は除外。
func LastHumanPrompt(agent, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", false
	}
	if st.Size() > transcriptTailBytes {
		if _, err := f.Seek(st.Size()-transcriptTailBytes, io.SeekStart); err != nil {
			return "", false
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	last := ""
	first := st.Size() > transcriptTailBytes // 途中から読む時は 1 行目が断片
	for sc.Scan() {
		line := sc.Bytes()
		if first {
			first = false
			continue
		}
		var txt string
		var ok bool
		switch agent {
		case "claude":
			txt, ok = claudeUserText(line)
		case "codex":
			txt, ok = codexUserText(line)
		default:
			return "", false
		}
		if ok {
			last = txt
		}
	}
	if last == "" {
		return "", false
	}
	return last, true
}

func isHumanText(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" || strings.HasPrefix(t, "<") {
		return false
	}
	if strings.HasPrefix(t, ReasonPrefix) || strings.HasPrefix(t, "Stop hook feedback:") {
		return false
	}
	return true
}

// claudeUserText は Claude Code transcript（JSONL）の 1 行から人間入力を取り出す。
// 行の形（2.1.312 実測）: {"type":"user","isMeta":true?,"message":{"role":"user",
// "content":"…" | [{"type":"text","text":"…"}|{"type":"tool_result",…}]}}
func claudeUserText(line []byte) (string, bool) {
	var r struct {
		Type    string `json:"type"`
		IsMeta  bool   `json:"isMeta"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &r); err != nil || r.Type != "user" || r.IsMeta {
		return "", false
	}
	var s string
	if err := json.Unmarshal(r.Message.Content, &s); err == nil {
		if isHumanText(s) {
			return s, true
		}
		return "", false
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(r.Message.Content, &parts); err != nil {
		return "", false
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "tool_result" {
			return "", false
		}
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	if isHumanText(b.String()) {
		return b.String(), true
	}
	return "", false
}

// codexUserText は Codex rollout（JSONL）の 1 行から人間入力を取り出す。
// 行の形（0.165.0 実測）: {"type":"response_item","payload":{"type":"message",
// "role":"user","content":[{"type":"input_text","text":"…"}]}}
func codexUserText(line []byte) (string, bool) {
	var r struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &r); err != nil || r.Type != "response_item" {
		return "", false
	}
	if r.Payload.Type != "message" || r.Payload.Role != "user" {
		return "", false
	}
	var b strings.Builder
	for _, c := range r.Payload.Content {
		b.WriteString(c.Text)
	}
	if isHumanText(b.String()) {
		return b.String(), true
	}
	return "", false
}
