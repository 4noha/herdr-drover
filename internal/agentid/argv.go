package agentid

// argv — resume 引数の抽出／再構築を ResumeSpec 駆動で行う。
//
// # 旧実装からの本質的な変更: 「値の書式」で判断しない
//
// 旧 stripResumeArgv は `--resume` の直後を **isUUID で判定**して落とすか決めて
// いた。claude の会話 ref はたまたま uuid だが、他のエージェントは違う
// （pi / omp は **path** も取る）。書式で判断すると
//   - path 形の ref を「値ではない」と誤判定して argv に残す（＝二重指定）
//   - 逆に uuid 風の無関係な引数を値と誤判定して落とす
// が起きる。**そのフラグが値を取るか**は Spec が知っている事実なので、
// そちらで決める（ヒューリスティック分類禁止の鉄則③）。
//
// # session ref の妥当性
//
// herdr 相当へ緩和する: 非空・512B 以下・制御文字なし。uuid 形は要求しない。

import "strings"

// ValidSessionRef は session ref として受け付けてよい値かを返す（herdr 相当）。
// 書式（uuid か path か）は問わない — それは agent ごとに違い、herdr が
// kind として別途持っている。
func ValidSessionRef(v string) bool {
	if v == "" || len(v) > 512 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// StripResume は argv から resume 指定を取り除く（agent の Spec に従う）。
// argv[0] は常に温存する。
//
// FormSubcommand（codex）は argv[1] がサブコマンドのときだけ、そこから 2 語
// （サブコマンド＋値）を落とす。値が無い（`codex resume` 単独＝picker 形）なら
// 1 語だけ落とす。
func StripResume(agent string, argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	sp := Resume(agent)
	if !sp.Supported {
		return append([]string(nil), argv...)
	}
	out := make([]string, 0, len(argv))
	out = append(out, argv[0])

	if sp.Form == FormSubcommand {
		i := 1
		if len(argv) > 1 && argv[1] == sp.Subcommand {
			i = 2
			// 直後が値ならそれも落とす。**次がフラグ（"-" 始まり）なら値ではない**
			// ＝ここは書式判定ではなく「フラグかどうか」の構造判定。
			if len(argv) > 2 && !strings.HasPrefix(argv[2], "-") {
				i = 3
			}
		}
		return append(out, argv[i:]...)
	}

	flags := append([]string{sp.Flag}, sp.Aliases...)
	isFlag := func(a string) bool {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
		return false
	}
	hasEqPrefix := func(a string) bool {
		for _, f := range flags {
			if strings.HasPrefix(a, f+"=") {
				return true
			}
		}
		return false
	}
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if isFlag(a) {
			// **そのフラグが値を取るか**で落とす／落とさないを決める（値の書式では
			// 決めない）。FormSpace なら次トークンが値。ただし次がフラグ形なら
			// 値なし（picker 形）とみなして残す。
			if sp.Form == FormSpace && i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
			}
			continue
		}
		if hasEqPrefix(a) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// BuildResume は argv に resume 指定を付け直す（既存の指定は先に落とす）。
// ref が空／不正、または agent が resume 非対応なら **argv を一切いじらない**
// （素起動へ落ちる。呼び手が loud に報告すること）。
func BuildResume(agent string, argv []string, ref string) []string {
	if len(argv) == 0 {
		return nil
	}
	sp := Resume(agent)
	if !sp.Supported || !ValidSessionRef(ref) {
		return append([]string(nil), argv...)
	}
	out := StripResume(agent, argv)
	switch sp.Form {
	case FormEquals:
		return append(out, sp.Flag+"="+ref)
	case FormSubcommand:
		// サブコマンドは argv[0] の直後（位置引数なので末尾に足しては効かない）。
		res := make([]string, 0, len(out)+2)
		res = append(res, out[0], sp.Subcommand, ref)
		return append(res, out[1:]...)
	default:
		return append(out, sp.Flag, ref)
	}
}

// SupportsKind は agent がその session ref kind を扱えるかを返す
// （herdr の agent_resume.rs の match 条件に対応）。
func SupportsKind(agent, kind string) bool {
	sp := Resume(agent)
	if !sp.Supported {
		return false
	}
	for _, k := range sp.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}
