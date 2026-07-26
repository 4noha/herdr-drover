// Package memvaultclient talks to a memvault daemon over its UNIX socket.
//
// It duplicates a small subset of github.com/4noha/memvault's internal
// client package (which drover cannot import because it's under internal/).
// Only the multi-owner subcommands (claim / release / whoami / status /
// issue-inherit-token) are supported — inject flows still belong on each
// operator's laptop and are out of scope for drover.
package memvaultclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultSocketPath returns the socket path a memvault daemon uses when it
// wasn't given an explicit --socket flag. Precedence:
//
//  1. MEMVAULT_SOCKET env var
//  2. $HOME/.memvault.sock (memvault's own default)
//
// If neither is set (rare — no HOME), returns an empty string; callers
// should treat that as "no memvault available" and skip gracefully.
func DefaultSocketPath() string {
	if v := os.Getenv("MEMVAULT_SOCKET"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".memvault.sock")
}

// Client is a small HTTP-over-UNIX-socket client for one memvault daemon.
type Client struct {
	SocketPath string
	// Timeout applies to each round-trip. 0 = 15s default.
	Timeout time.Duration
}

// New returns a Client for the given socket path. Pass "" to use DefaultSocketPath().
func New(sock string) *Client {
	if sock == "" {
		sock = DefaultSocketPath()
	}
	return &Client{SocketPath: sock}
}

func (c *Client) httpClient() *http.Client {
	t := c.Timeout
	if t == 0 {
		t = 15 * time.Second
	}
	sock := c.SocketPath
	return &http.Client{
		Timeout: t,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}
}

// doJSON runs one request and returns the response body plus HTTP status.
// The URL host is ignored (UNIX socket); we pass "vault" as a placeholder.
func (c *Client) doJSON(method, path string, body io.Reader) ([]byte, int, error) {
	if c.SocketPath == "" {
		return nil, 0, errors.New("memvault: no socket path (MEMVAULT_SOCKET unset and no HOME)")
	}
	req, err := http.NewRequest(method, "http://vault"+path, body)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	return buf, resp.StatusCode, err
}

// Status is the parsed shape of GET /status that we need to know about.
// Extra fields the daemon returns are ignored (forward-compatible).
type Status struct {
	UptimeSec        int                       `json:"uptime_sec"`
	HardCapRemainSec int                       `json:"hard_cap_remain_sec"`
	GCPLoaded        bool                      `json:"gcp_loaded"`
	AWSLoaded        bool                      `json:"aws_loaded"`
	EnvLoaded        bool                      `json:"env_loaded"`
	EnvNames         []string                  `json:"env_names"`
	RoutesLoaded     bool                      `json:"routes_loaded"`
	Slots            map[string]map[string]any `json:"slots"`
	ActiveOperator   string                    `json:"active_operator"`
	ActiveSlot       string                    `json:"active_slot"`
	InheritInPlace   bool                      `json:"inherit_in_place"`
}

// Status calls GET /status and returns the parsed result.
func (c *Client) Status() (*Status, error) {
	buf, code, err := c.doJSON("GET", "/status", nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, fmt.Errorf("memvault /status returned %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var st Status
	if err := json.Unmarshal(buf, &st); err != nil {
		return nil, fmt.Errorf("parse /status: %w", err)
	}
	return &st, nil
}

// Whoami is the parsed shape of GET /whoami.
type Whoami struct {
	ActiveOperator string `json:"active_operator"`
	ActiveSlot     string `json:"active_slot"`
	InheritInPlace bool   `json:"inherit_in_place"`
}

func (c *Client) Whoami() (*Whoami, error) {
	buf, code, err := c.doJSON("GET", "/whoami", nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return nil, fmt.Errorf("memvault /whoami returned %d: %s", code, strings.TrimSpace(string(buf)))
	}
	var wr Whoami
	if err := json.Unmarshal(buf, &wr); err != nil {
		return nil, fmt.Errorf("parse /whoami: %w", err)
	}
	return &wr, nil
}

// ClaimOptions holds the parameters to POST /claim.
type ClaimOptions struct {
	Operator string
	Force    bool
	Inherit  bool
	Token    string // required when Inherit=true
}

// Claim sets the active operator.
//
// Conflict (HTTP 409, "another operator is currently active") is returned
// as a distinguishable error via errors.Is(ErrClaimConflict).
func (c *Client) Claim(opts ClaimOptions) ([]byte, error) {
	if opts.Operator == "" {
		return nil, errors.New("Claim: Operator required")
	}
	if opts.Inherit && opts.Token == "" {
		return nil, errors.New("Claim: Inherit requires Token")
	}
	q := "operator=" + urlEscape(opts.Operator)
	if opts.Force {
		q += "&force=1"
	}
	if opts.Inherit {
		q += "&inherit=1&token=" + urlEscape(opts.Token)
	}
	buf, code, err := c.doJSON("POST", "/claim?"+q, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusConflict {
		return buf, fmt.Errorf("%w: %s", ErrClaimConflict, strings.TrimSpace(string(buf)))
	}
	if code/100 != 2 {
		return buf, fmt.Errorf("memvault /claim returned %d: %s", code, strings.TrimSpace(string(buf)))
	}
	return buf, nil
}

// ErrClaimConflict is the sentinel returned by Claim when the daemon
// refuses because another operator is active (and Force wasn't set).
var ErrClaimConflict = errors.New("memvault claim conflict")

// Release clears the active operator. Operator/Force follow the same
// semantics as the daemon's /release.
func (c *Client) Release(operator string, force bool) ([]byte, error) {
	q := ""
	if operator != "" {
		q = "operator=" + urlEscape(operator)
	}
	if force {
		if q != "" {
			q += "&"
		}
		q += "force=1"
	}
	path := "/release"
	if q != "" {
		path += "?" + q
	}
	buf, code, err := c.doJSON("POST", path, nil)
	if err != nil {
		return nil, err
	}
	if code == http.StatusConflict {
		return buf, fmt.Errorf("%w: %s", ErrReleaseConflict, strings.TrimSpace(string(buf)))
	}
	if code/100 != 2 {
		return buf, fmt.Errorf("memvault /release returned %d: %s", code, strings.TrimSpace(string(buf)))
	}
	return buf, nil
}

// ErrReleaseConflict is the sentinel returned by Release when the caller
// doesn't match the current active operator (and Force wasn't set).
var ErrReleaseConflict = errors.New("memvault release conflict")

// IssueInheritToken calls POST /issue-inherit-token.
// ttl "" leaves the daemon's default.
func (c *Client) IssueInheritToken(owner, forOp, ttl string) ([]byte, error) {
	if owner == "" {
		return nil, errors.New("IssueInheritToken: owner required")
	}
	q := "owner=" + urlEscape(owner)
	if forOp != "" {
		q += "&for=" + urlEscape(forOp)
	}
	if ttl != "" {
		q += "&ttl=" + urlEscape(ttl)
	}
	buf, code, err := c.doJSON("POST", "/issue-inherit-token?"+q, nil)
	if err != nil {
		return nil, err
	}
	if code/100 != 2 {
		return buf, fmt.Errorf("memvault /issue-inherit-token returned %d: %s", code, strings.TrimSpace(string(buf)))
	}
	return buf, nil
}

// urlEscape is a compact url.QueryEscape replacement to avoid the net/url
// dependency (this file is small and testable without it). Handles the
// same chars we actually meet here (names / tokens / durations).
func urlEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '-' || r == '_' || r == '.' || r == '~' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", r)
	}
	return b.String()
}
