package main

// memvault サブコマンド: drover から共用 slave 上の memvault daemon (4noha/memvault)
// を叩くための thin wrapper。 memvault 本体の CLI と同じ操作は memvault 自体で
// できるが、drover は「今 pane に紐付いた operator は誰か」「reconcile の観点で
// 危ないタイミングはないか」といった herdr world の文脈を持てるので、operator
// 切替を drover 経由で行うのが自然。
//
// scope:
//   - herdr-drover memvault status   → memvault /status を pretty-print
//   - herdr-drover memvault whoami   → active operator を表示
//   - herdr-drover memvault claim    → 自分の名前で claim（force / inherit 対応）
//   - herdr-drover memvault release  → 自分を release
//   - herdr-drover memvault issue-inherit-token
//
// 未 scope（意図的）:
//   - inject 系: raw material を送る経路は各 operator の laptop からの SSH tunnel
//     が担当。drover は inject 経路を持たない。
//   - proxy / metadata / credential_process 経路: これは memvault 本体の責務で、
//     drover を経由する必要は無い（$MEMVAULT_SOCKET / $GCE_METADATA_HOST / AWS の
//     credential_process が pane env で解決する）。

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/4noha/herdr-drover/internal/memvaultclient"
)

func cmdMemvault(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: herdr-drover memvault <status|whoami|claim|release|issue-inherit-token>")
		return 2
	}
	sub, rest := args[0], args[1:]
	var err error
	switch sub {
	case "status":
		err = memvaultStatus(rest, stdout, stderr)
	case "whoami":
		err = memvaultWhoami(rest, stdout, stderr)
	case "claim":
		err = memvaultClaim(rest, stdout, stderr)
	case "release":
		err = memvaultRelease(rest, stdout, stderr)
	case "issue-inherit-token":
		err = memvaultIssueInheritToken(rest, stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, memvaultHelp)
		return 0
	default:
		fmt.Fprintf(stderr, "herdr-drover memvault: 未知のサブコマンド %q\n%s", sub, memvaultHelp)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "herdr-drover memvault %s: %v\n", sub, err)
		if errors.Is(err, memvaultclient.ErrClaimConflict) || errors.Is(err, memvaultclient.ErrReleaseConflict) {
			return 3 // conflict は 3 (2 は usage・1 は generic runtime）
		}
		return 1
	}
	return 0
}

const memvaultHelp = `herdr-drover memvault — memvault daemon (4noha/memvault) への thin wrapper

Usage:
  herdr-drover memvault status                              memvault /status を pretty-print
  herdr-drover memvault whoami                              active operator を表示
  herdr-drover memvault claim   [--operator NAME] [--force] [--inherit --token T]
  herdr-drover memvault release [--operator NAME] [--force]
  herdr-drover memvault issue-inherit-token --owner NAME [--for OP] [--ttl 8h]

Environment:
  MEMVAULT_SOCKET   memvault daemon の UNIX socket path（既定 $HOME/.memvault.sock）

Notes:
  - inject 系は各 operator の laptop で行う設計なので drover 経由の入口は
    意図的に無い（raw material は laptop から出さない）。
  - --operator 未指定は $MEMVAULT_OPERATOR、無ければ $USER をフォールバックする。
`

// operatorDefault は claim/release の --operator 省略時のフォールバック順序。
// pane env に memvault 用に運用者の名前を宣言する余地を残しつつ、最後は $USER。
func operatorDefault() string {
	if v := os.Getenv("MEMVAULT_OPERATOR"); v != "" {
		return v
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return ""
}

func memvaultStatus(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := memvaultclient.New("")
	if c.SocketPath == "" {
		return errors.New("$MEMVAULT_SOCKET も $HOME/.memvault.sock も見つからない")
	}
	st, err := c.Status()
	if err != nil {
		return err
	}
	// pretty-print
	buf, _ := json.MarshalIndent(st, "", "  ")
	fmt.Fprintln(stdout, string(buf))
	return nil
}

func memvaultWhoami(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := memvaultclient.New("")
	if c.SocketPath == "" {
		return errors.New("$MEMVAULT_SOCKET も $HOME/.memvault.sock も見つからない")
	}
	w, err := c.Whoami()
	if err != nil {
		return err
	}
	if w.ActiveOperator == "" {
		fmt.Fprintln(stdout, "active_operator=(none) → default slot only")
	} else if w.InheritInPlace {
		fmt.Fprintf(stdout, "active_operator=%s (inheriting slot %q)\n", w.ActiveOperator, w.ActiveSlot)
	} else {
		fmt.Fprintf(stdout, "active_operator=%s\n", w.ActiveOperator)
	}
	return nil
}

func memvaultClaim(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	op := fs.String("operator", "", "operator name (省略時 $MEMVAULT_OPERATOR → $USER)")
	force := fs.Bool("force", false, "他 operator が active でも奪い取る（旧 slot を wipe）")
	inherit := fs.Bool("inherit", false, "他人の slot を借りる（--token 必須）")
	token := fs.String("token", "", "inherit-token (--inherit 必須)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	operator := *op
	if operator == "" {
		operator = operatorDefault()
	}
	if operator == "" {
		return errors.New("operator 名を決定できない（--operator も $MEMVAULT_OPERATOR も $USER も空）")
	}
	c := memvaultclient.New("")
	buf, err := c.Claim(memvaultclient.ClaimOptions{
		Operator: operator, Force: *force, Inherit: *inherit, Token: *token,
	})
	if err != nil {
		// claim conflict は memvault からの JSON を stderr に出しつつ error で終わる
		if errors.Is(err, memvaultclient.ErrClaimConflict) {
			fmt.Fprintln(stderr, prettyJSONOrRaw(buf))
		}
		return err
	}
	fmt.Fprintln(stdout, prettyJSONOrRaw(buf))
	return nil
}

func memvaultRelease(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	op := fs.String("operator", "", "operator name (省略時 $MEMVAULT_OPERATOR → $USER)")
	force := fs.Bool("force", false, "active operator と一致しなくても release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	operator := *op
	if operator == "" {
		operator = operatorDefault()
	}
	c := memvaultclient.New("")
	buf, err := c.Release(operator, *force)
	if err != nil {
		if errors.Is(err, memvaultclient.ErrReleaseConflict) {
			fmt.Fprintln(stderr, prettyJSONOrRaw(buf))
		}
		return err
	}
	fmt.Fprintln(stdout, prettyJSONOrRaw(buf))
	return nil
}

func memvaultIssueInheritToken(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("issue-inherit-token", flag.ContinueOnError)
	owner := fs.String("owner", "", "誰の slot を貸すか（自分の名前が入るべき）")
	forOp := fs.String("for", "", "特定 operator に pin（省略時は誰でも受理）")
	ttl := fs.String("ttl", "8h", "token の寿命 (default 8h, max 24h)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *owner == "" {
		return errors.New("--owner is required")
	}
	c := memvaultclient.New("")
	buf, err := c.IssueInheritToken(*owner, *forOp, *ttl)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, prettyJSONOrRaw(buf))
	return nil
}

// prettyJSONOrRaw returns buf pretty-printed if it parses as JSON, else the
// trimmed raw string. Used to display daemon responses.
func prettyJSONOrRaw(buf []byte) string {
	var v any
	if err := json.Unmarshal(buf, &v); err != nil {
		return strings.TrimSpace(string(buf))
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return strings.TrimSpace(string(buf))
	}
	return string(out)
}
