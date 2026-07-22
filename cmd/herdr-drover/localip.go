package main

import (
	"net"
	"sort"
)

// localIPs は自 PC の全ローカル IP アドレス（loopback 除外・IPv4/IPv6 両方）を
// 決定的な順序（文字列昇順）で返す。実運用要望「SSH 到達先確認のため各 tab に
// IP を出したい」への対処（producer.WithLocalIPs 経由で session.BuildSessions
// が local_ips として Firestore へ載せる。DROVER_SHARE_LOCAL_IPS opt-out 時は
// この関数自体を呼ばない＝cmd 側の呼び出し判断は agent.go 参照）。
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
	sort.Strings(out)
	return out
}
