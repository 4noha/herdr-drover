// Package transcripthist は全画面 TUI のエージェント（Claude Code / Codex）の会話履歴を、
// そのエージェント自身の transcript（JSONL）から素のテキスト行に組み立てる。
//
// # なぜ要るか
//
// これらのエージェントは画面を描き直す形で動くため、herdr の scrollback には
// 可視画面ぶんしか残らない（実測 2026-10-30: 全 pane で pane.read recent が
// viewport_rows+1 行・max_offset_from_bottom=0。herdr のヘルプにも「alternate-screen の
// 行は通常の scrollback に入らない」とある）。ワイヤ v2 の履歴（pane.read ベース）は
// そのままでは 0 行になる。会話の実体は transcript に残っているので、そこから組み立てる。
//
// transcript は巨大になりうる（Codex の rollout は 1.7GB の実例）ため、末尾
// tailBytes だけを読む。
package transcripthist

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ErrNotFound は transcript が見つからない（未対応エージェント・sid 不明を含む）。
var ErrNotFound = errors.New("transcript not found")

const (
	// tailBytes は transcript の末尾読取量。
	tailBytes = 24 << 20
	// toolArgMax はツール呼出の要約の最大文字数。
	toolArgMax = 100
)

// Supported はこの agent 種別を組み立てられるか。
func Supported(agent string) bool { return agent == "claude" || agent == "codex" }

// Find は agent の会話 id（herdr の agent_session.value）から transcript のパスを返す。
// home は HOME（テストで差し替える）。
func Find(agent, sid, home string) (string, error) {
	if sid == "" || strings.ContainsAny(sid, `*?[/\`) {
		return "", ErrNotFound
	}
	var pat string
	switch agent {
	case "claude":
		pat = filepath.Join(home, ".claude", "projects", "*", sid+".jsonl")
	case "codex":
		pat = filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*-"+sid+".jsonl")
	default:
		return "", ErrNotFound
	}
	m, err := filepath.Glob(pat)
	if err != nil || len(m) == 0 {
		return "", ErrNotFound
	}
	best, bt := "", int64(-1)
	for _, p := range m { // 同じ id が複数あれば最新
		if st, err := os.Stat(p); err == nil && st.ModTime().UnixNano() > bt {
			best, bt = p, st.ModTime().UnixNano()
		}
	}
	return best, nil
}

// Render は transcript の末尾から会話を素のテキスト行にして、末尾 maxLines 行を返す。
// 行の形: ユーザ「> …」・エージェント「● …」・ツール呼出「  ⎿ 名前 要約」。
func Render(agent, path string, maxLines int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	partial := false
	if st.Size() > tailBytes {
		if _, err := f.Seek(st.Size()-tailBytes, io.SeekStart); err != nil {
			return "", err
		}
		partial = true // 先頭の 1 行は断片
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	var blocks []block
	for sc.Scan() {
		if partial {
			partial = false
			continue
		}
		var bs []block
		switch agent {
		case "claude":
			bs = claudeBlocks(sc.Bytes())
		case "codex":
			bs = codexBlocks(sc.Bytes())
		default:
			return "", ErrNotFound
		}
		blocks = append(blocks, bs...)
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	var lines []string
	for _, b := range blocks {
		lines = append(lines, b.lines()...)
	}
	if len(lines) > maxLines && maxLines > 0 {
		lines = lines[len(lines)-maxLines:]
	}
	if len(lines) == 0 {
		return "", ErrNotFound
	}
	return strings.Join(lines, "\n") + "\n", nil
}

type blockKind int

const (
	kUser blockKind = iota
	kAssistant
	kTool
)

type block struct {
	kind blockKind
	text string
}

func (b block) lines() []string {
	text := strings.TrimRight(b.text, " \r\n\t")
	if text == "" {
		return nil
	}
	ls := strings.Split(text, "\n")
	switch b.kind {
	case kTool:
		return []string{"  ⎿ " + ls[0]}
	case kUser:
		out := []string{"", "> " + ls[0]}
		for _, l := range ls[1:] {
			out = append(out, "  "+l)
		}
		return out
	default:
		out := []string{"", "● " + ls[0]}
		for _, l := range ls[1:] {
			out = append(out, "  "+l)
		}
		return out
	}
}

// humanText は環境注入（`<` 始まり・AGENTS.md の自動前置き）を除いた人の入力か。
func humanText(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if t == "" || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "# AGENTS.md instructions") {
		return "", false
	}
	return t, true
}

func claudeBlocks(line []byte) []block {
	var r struct {
		Type        string `json:"type"`
		IsMeta      bool   `json:"isMeta"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &r); err != nil || r.IsMeta || r.IsSidechain {
		return nil
	}
	if r.Type != "user" && r.Type != "assistant" {
		return nil
	}
	var s string
	if json.Unmarshal(r.Message.Content, &s) == nil {
		if r.Type == "user" {
			if t, ok := humanText(s); ok {
				return []block{{kUser, t}}
			}
			return nil
		}
		return []block{{kAssistant, s}}
	}
	var parts []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(r.Message.Content, &parts) != nil {
		return nil
	}
	var out []block
	for _, p := range parts {
		switch {
		case p.Type == "text" && r.Type == "user":
			if t, ok := humanText(p.Text); ok {
				out = append(out, block{kUser, t})
			}
		case p.Type == "text":
			out = append(out, block{kAssistant, p.Text})
		case p.Type == "tool_use":
			out = append(out, block{kTool, toolLine(p.Name, p.Input)})
		}
	}
	return out
}

func codexBlocks(line []byte) []block {
	var r struct {
		Type    string `json:"type"`
		Payload struct {
			Type      string `json:"type"`
			Role      string `json:"role"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Input     string `json:"input"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(line, &r); err != nil || r.Type != "response_item" {
		return nil
	}
	p := r.Payload
	switch p.Type {
	case "message":
		var b strings.Builder
		for _, c := range p.Content {
			b.WriteString(c.Text)
		}
		switch p.Role {
		case "user":
			if t, ok := humanText(b.String()); ok {
				return []block{{kUser, t}}
			}
		case "assistant":
			return []block{{kAssistant, b.String()}}
		}
	case "function_call":
		return []block{{kTool, toolLine(p.Name, json.RawMessage(p.Arguments))}}
	case "custom_tool_call":
		return []block{{kTool, toolLine(p.Name, mustJSONString(p.Input))}}
	}
	return nil
}

func mustJSONString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// toolLine は「名前 要約」の 1 行。入力の主要キーがあればその値、なければ最初の文字列値。
func toolLine(name string, input json.RawMessage) string {
	if name == "" {
		name = "tool"
	}
	sum := summarizeInput(input)
	if sum == "" {
		return name
	}
	return name + " " + sum
}

var toolKeys = []string{"command", "cmd", "file_path", "path", "pattern", "url", "description", "query", "prompt"}

func summarizeInput(input json.RawMessage) string {
	var s string
	if json.Unmarshal(input, &s) == nil {
		return oneLine(s)
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range toolKeys {
		if v, ok := m[k].(string); ok && v != "" {
			return oneLine(v)
		}
	}
	for _, v := range m {
		if v, ok := v.(string); ok && v != "" {
			return oneLine(v)
		}
	}
	return ""
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > toolArgMax {
		r := []rune(s)
		s = string(r[:toolArgMax]) + "…"
	}
	return s
}
