package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// virtualIfacePrefixes は churn の原因になる仮想 NIC の名前 prefix。
// 敵対的レビュー指摘: Docker/Podman/Colima の bridge や veth が container 起動
// のたびに up/down して IPv4 集合が変わり、真の NIC 変化でないのに watchLifecycle
// が発火し続ける（cooldown より長い間隔だと毎回発火するため設計上抑制できない）。
// これらは実 relay 接続経路には関与しないので fingerprint 対象から除外する。
// macOS の bridge*/utun* は Tailscale/VPN で正常に up されるものも含むため
// 除外しない（VPN toggle は本来検知したい変化）。
var virtualIfacePrefixes = []string{
	"docker",  // Linux docker0 / docker_gwbridge
	"br-",     // Linux docker user-defined bridge (br-<hex>)
	"veth",    // Linux container veth pair endpoints
	"cni",     // Kubernetes CNI (cni0 等)
	"flannel", // Flannel VXLAN
	"weave",   // Weave Net
	"podman",  // Podman bridge
	"kube",    // kube-bridge, kube-ipvs0 等
	"vmnet",   // VMware Fusion / Colima 系
}

// isVirtualIface は Docker/Podman/K8s の仮想 NIC を判定する（fingerprint 除外用）。
func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// nicFingerprint は自 PC の NIC 変化検知用の軽量なキーを返す（attach.go の
// watchLifecycle が Wi-Fi 切替 / VPN toggle / 有線抜差し等を検知するために使う）。
// localIPs() を再利用しない理由: localIPs は tailscale CLI を fork/exec するため
// (最悪 2s ブロック) 検知ループから短周期で呼ぶには重すぎる。また
// tailscale MagicDNS 名は NIC 変化とは無関係の情報。
//
// IPv6 の SLAAC privacy address (RFC 4941) は数時間おきに新旧が入れ替わり、
// NIC が変わっていないのに address 集合が変わる＝誤検知の元。Go の
// net.InterfaceAddrs() は temporary/deprecated flag を露出しないため IPv6 の
// 一時アドレスを厳密に弾く手段が無い。従って **本キーは IPv4 のみを対象に
// する**（Wi-Fi/有線切替は DHCP で IPv4 が必ず変わるので実用上十分検知できる。
// 純 IPv6-only 環境は現状想定外）。
//
// Docker/Podman/K8s bridge は fingerprint から除外（敵対的レビュー指摘への対処。
// container 起動のたびに up/down する変化で毎回 forceClose するのを防ぐ）。
// virtualIfacePrefixes 参照。net.Interfaces() ベースにするのは interface 名を
// 得るため（net.InterfaceAddrs() は addrs のみで name 情報が無い）。
//
// 順序保証: interface enumeration 順は不定なので sort して canonical 化。
// IPNet.String() は "10.101.71.143/24" 形式で prefix 幅も含む＝同じ IP でも mask
// 変化を拾える。呼出コスト: net.Interfaces() + 各 iface の Addrs() で netlink/
// sysctl round-trip が数回、通常 sub-ms。
//
// net.Interfaces() 失敗は "" を返す（呼び手は「前回値と同じ空」として扱う
// ＝短期の一時失敗で誤発火しない）。
func nicFingerprint() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	keys := make([]string, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // down は対象外
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualIface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			v4 := ipnet.IP.To4()
			if v4 == nil {
				continue // IPv6 は SLAAC privacy address 誤検知回避のため除外
			}
			ones, _ := ipnet.Mask.Size()
			if ones == 0 || ones > 32 {
				ones = 32
			}
			keys = append(keys, fmt.Sprintf("%s/%d", v4.String(), ones))
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
