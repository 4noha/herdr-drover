package autocontinue

// 判定事例の蓄積（cases.jsonl）とラベル（labels.jsonl）。
//
// # なぜ要るか
//
// 判定ルールは「合理的だが実データで外れる」ことがある（2026-10-29 gts: 本文の技術用語
// "secret" で「続けますか？」を人待ちと誤判定）。修正前のルールも筋は通っていたので、
// 個別の修正を重ねるより**実事例を貯めて全体整合性のあるテストコーパス**にするのが
// 改善の近道。ここでは hook が判定するたびに材料と結果を 1 行ずつ残し、次の呼出で
// 「人がどう反応したか」（停止後に『つづけて』を打ったか等）を追記する。
//
// # 置き場と扱い
//
//   - <dir>/cases.jsonl  … 1 判定 1 行（最終発話は末尾 4000 字・指示は 1000 字に切る）。
//     会話内容を含むので**ローカル限定**（リポジトリへは `auto cases export` で
//     人が選んだものだけを testdata/judge_cases.json へ持ち込む）。
//   - <dir>/labels.jsonl … 人が付けた正解ラベル（`auto cases label <id> <continue|human|done>`）。
//   - 事例には RulesVersion（judge.go）を刻む＝どの版のルールの判定かが後から分かる。
//     ルール変更の経緯は RULES_CHANGELOG.md。
//
// 追記のみ・読取は末尾から N 件。失敗は呼び手へ返すだけで判定を止めない。

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Case は 1 回の判定記録。
type Case struct {
	ID       string `json:"id"` // <agent>-<sid 末尾 8>-<unix ms>
	At       string `json:"at"`
	Rules    int    `json:"rules"` // RulesVersion
	Agent    string `json:"agent"`
	Pane     string `json:"pane"`
	SID      string `json:"sid"`
	Active   bool   `json:"active"` // stop_hook_active
	Grant    bool   `json:"grant"`
	Asked    bool   `json:"asked"` // 直近の指示が続行系
	Count    int    `json:"count"`
	Cap      int    `json:"cap"`
	Prompt   string `json:"prompt"`   // 直近の人間の指示（末尾 1000 字）
	Message  string `json:"message"`  // 最終発話（末尾 4000 字）
	Verdict  string `json:"verdict"`  // continue / human / done
	Source   string `json:"source"`   // rule:… / llm / fallback:…
	Detail   string `json:"detail"`   // ルールの一致箇所など
	Decision string `json:"decision"` // continue / stop / cap
	Reason   string `json:"reason"`   // 送った続行文（続行時）
	Skipped  string `json:"skipped,omitempty"`
	// 以下は後から追記される（次の hook 呼出・人のラベル）。読取時に合成する。
	FollowupPrompt   string `json:"followup_prompt,omitempty"`   // 停止後に人が打った最初の指示
	FollowupContinue *bool  `json:"followup_continue,omitempty"` // それが続行系だったか（停止が誤りだった示唆）
	Label            string `json:"label,omitempty"`             // 人の正解ラベル
	LabelNote        string `json:"label_note,omitempty"`
}

// followup は停止後の人の反応（cases.jsonl に kind=followup で追記）。
type followupRec struct {
	Kind     string `json:"kind"`
	CaseID   string `json:"case_id"`
	At       string `json:"at"`
	Prompt   string `json:"prompt"`
	Continue bool   `json:"continue"`
}

// labelRec は人のラベル（labels.jsonl）。
type labelRec struct {
	CaseID string `json:"case_id"`
	At     string `json:"at"`
	Label  string `json:"label"`
	Note   string `json:"note,omitempty"`
}

const (
	caseMessageMax = 4000
	casePromptMax  = 1000
)

func (s *Store) casesPath() string  { return filepath.Join(s.Dir, "cases.jsonl") }
func (s *Store) labelsPath() string { return filepath.Join(s.Dir, "labels.jsonl") }

// CasesPath は蓄積ファイルの場所（status 表示用）。
func (s *Store) CasesPath() string { return s.casesPath() }

func appendJSONL(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// NewCaseID は事例 id を作る（同一ミリ秒の衝突を避けるため乱数 4 桁を付ける）。
func NewCaseID(agent, sid string, at time.Time) string {
	tail := sid
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	var r [2]byte
	_, _ = rand.Read(r[:])
	return fmt.Sprintf("%s-%s-%d-%x", agent, tail, at.UnixMilli(), r)
}

// AppendCase は判定記録を 1 行追記する（本文は切り詰める）。
func (s *Store) AppendCase(c Case) error {
	c.Message = tail(c.Message, caseMessageMax)
	c.Prompt = tail(c.Prompt, casePromptMax)
	return appendJSONL(s.casesPath(), c)
}

// AppendFollowup は「停止した事例の後に人が打った最初の指示」を追記する。
func (s *Store) AppendFollowup(caseID, prompt string, at time.Time) error {
	return appendJSONL(s.casesPath(), followupRec{Kind: "followup", CaseID: caseID, At: at.Format(time.RFC3339),
		Prompt: tail(prompt, casePromptMax), Continue: ContinueLike(prompt)})
}

// LabelCase は人の正解ラベルを追記する（continue / human / done）。
func (s *Store) LabelCase(caseID, label, note string, at time.Time) error {
	switch label {
	case "continue", "human", "done":
	default:
		return fmt.Errorf("label は continue|human|done（%q）", label)
	}
	return appendJSONL(s.labelsPath(), labelRec{CaseID: caseID, At: at.Format(time.RFC3339), Label: label, Note: note})
}

// LastCaseOf は同じ agent+sid の直近の判定記録（無ければ ok=false）。
func (s *Store) LastCaseOf(agent, sid string) (Case, bool) {
	cases, err := s.ReadCases(0)
	if err != nil {
		return Case{}, false
	}
	for i := len(cases) - 1; i >= 0; i-- {
		if cases[i].Agent == agent && cases[i].SID == sid {
			return cases[i], true
		}
	}
	return Case{}, false
}

// ReadCases は事例を時系列で返す（followup とラベルを合成済み）。n>0 なら末尾 n 件。
func (s *Store) ReadCases(n int) ([]Case, error) {
	f, err := os.Open(s.casesPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cases []Case
	idx := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(line, &probe)
		if probe.Kind == "followup" {
			var fr followupRec
			if err := json.Unmarshal(line, &fr); err == nil {
				if i, ok := idx[fr.CaseID]; ok {
					cases[i].FollowupPrompt = fr.Prompt
					c := fr.Continue
					cases[i].FollowupContinue = &c
				}
			}
			continue
		}
		var c Case
		if err := json.Unmarshal(line, &c); err != nil {
			continue // 壊れた行は飛ばす（追記専用ファイル＝途中の 1 行で全体を落とさない）
		}
		idx[c.ID] = len(cases)
		cases = append(cases, c)
	}
	if lf, err := os.Open(s.labelsPath()); err == nil {
		defer lf.Close()
		ls := bufio.NewScanner(lf)
		for ls.Scan() {
			var lr labelRec
			if err := json.Unmarshal(ls.Bytes(), &lr); err == nil {
				if i, ok := idx[lr.CaseID]; ok {
					cases[i].Label, cases[i].LabelNote = lr.Label, lr.Note
				}
			}
		}
	}
	if n > 0 && len(cases) > n {
		cases = cases[len(cases)-n:]
	}
	return cases, nil
}

// CorpusCase はリポジトリ内コーパス（testdata/judge_cases.json）の 1 件。ローカルの
// Case から人が選んで export し、期待ラベルを付けたもの。
type CorpusCase struct {
	ID     string `json:"id"`
	Origin string `json:"origin"` // どこから来た事例か（セッション・日付・何が起きたか）
	Added  string `json:"added"`  // 追加日
	Rules  int    `json:"rules"`  // 追加時の RulesVersion
	Agent  string `json:"agent,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Grant  bool   `json:"grant,omitempty"`
	Asked  bool   `json:"asked,omitempty"`
	// Message は最終発話（コーパスでは原文のまま。個人情報・秘密は export 時に人が落とす）。
	Message string `json:"message"`
	// Expect は期待する判定: continue / human / done。ExpectSource はルールで決まるべき
	// 事例ならその source（"rule:…"）、判定器に委ねる事例なら "llm"。
	Expect       string `json:"expect"`
	ExpectSource string `json:"expect_source"`
}

// ExportCorpus はローカル事例を CorpusCase に写す（期待は人が後で埋める。ラベル済みなら
// それを初期値にする）。
func ExportCorpus(c Case, origin string, at time.Time) CorpusCase {
	exp := c.Label
	if exp == "" {
		exp = c.Verdict
	}
	src := c.Source
	if !strings.HasPrefix(src, "rule:") {
		src = "llm"
	}
	return CorpusCase{ID: c.ID, Origin: origin, Added: at.Format("2006-01-02"), Rules: RulesVersion, Agent: c.Agent,
		Prompt: c.Prompt, Grant: c.Grant, Asked: c.Asked, Message: c.Message, Expect: exp, ExpectSource: src}
}
