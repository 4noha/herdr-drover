package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// localIPs は自 PC の全ローカル IP アドレス（loopback 除外・IPv4/IPv6 両方。
// Tailscale 等の CGNAT レンジ 100.64.0.0/10 も他の仮想 NIC と同様に含む＝
// 特別扱いしない）＋利用可能なら Tailscale MagicDNS 名を決定的な順序
// （文字列昇順）で返す。実運用要望「SSH 到達先確認のため各 tab に IP/DNS 名を
// 出したい。ノート機は Tailscale の MagicIP や Domain name も持つことが多い」
// への対処（producer.WithLocalIPs 経由で session.BuildSessions が local_ips
// として Firestore へ載せる。DROVER_SHARE_LOCAL_IPS opt-out 時はこの関数自体を
// 呼ばない＝cmd 側の呼び出し判断は agent.go 参照）。
// net.InterfaceAddrs 失敗は空を返す（致命ではない・IP 一覧はベストエフォート
// の付加情報＝session 本体の push を止めない）。
func localIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		out = append(out, ipnet.IP.String())
	}
	if name := tailscaleMagicDNSName(); name != "" {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// tailscaleDNSNameTimeout は tailscale CLI 呼び出しの上限。ローカル daemon への
// unix socket 通信のみで通常は数十 ms 未満だが、daemon が不調で応答が遅い場合に
// producer.Tick（既定 5s 周期）を長時間ブロックしないための保険。
const tailscaleDNSNameTimeout = 2 * time.Second

// tailscaleStatusSelf は `tailscale status --json` の必要部分のみ（未知フィールド
// は無視＝Decode 時に破壊的エラーにしない。他フィールドは一切使わないため
// DisallowUnknownFields はしない）。
type tailscaleStatusSelf struct {
	Self struct {
		DNSName string `json:"DNSName"`
	} `json:"Self"`
}

// tailscaleAppBundleBin は macOS App Store 版 Tailscale.app のバイナリ
// （PATH 未登録・standalone tailscaled/tailscale CLI が別途無い環境向けの
// fallback）。実機確認: このパスの `status --json` は標準 CLI と byte 互換の
// JSON を返す。
const tailscaleAppBundleBin = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// tailscaleMagicDNSName は Tailscale の `status --json` から MagicDNS 名
// （末尾の "." を除いた FQDN、例 "host.tailnet-name.ts.net"）を返す。まず PATH の
// `tailscale` CLI（Linux/標準インストール）を試し、無ければ macOS App Store 版の
// バンドル内バイナリを試す。両方失敗・daemon 未接続・JSON 不正・DNSName 未設定は
// いずれも ""（ベストエフォートで諦める。IP 一覧そのものは net.InterfaceAddrs 側
// で既に取れているため、この関数が失敗しても付加情報が 1 つ減るだけで実害はない）。
func tailscaleMagicDNSName() string {
	bin := "tailscale"
	if _, err := exec.LookPath(bin); err != nil {
		if fi, serr := os.Stat(tailscaleAppBundleBin); serr != nil || fi.IsDir() {
			return ""
		}
		bin = tailscaleAppBundleBin
	}
	ctx, cancel := context.WithTimeout(context.Background(), tailscaleDNSNameTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil {
		return ""
	}
	return parseTailscaleDNSName(out)
}

// parseTailscaleDNSName は `tailscale status --json` の生バイト列から MagicDNS
// 名を抽出する純関数（tailscaleMagicDNSName から切り出し・外部プロセス無しで
// 単体テスト可能にする）。壊れた JSON・DNSName 未設定はいずれも ""。
func parseTailscaleDNSName(statusJSON []byte) string {
	var st tailscaleStatusSelf
	if err := json.Unmarshal(statusJSON, &st); err != nil {
		return ""
	}
	return strings.TrimSuffix(st.Self.DNSName, ".")
}
