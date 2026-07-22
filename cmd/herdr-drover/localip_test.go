package main

import "testing"

// TestParseTailscaleDNSNameStripsTrailingDot は実測データ（このマシンで
// Tailscale を有効化して `tailscale status --json` / App Store 版バンドル
// バイナリ経由で採取した実 JSON の該当部分）で、DNSName の末尾 "." が
// 除去されることを確認する。
func TestParseTailscaleDNSNameStripsTrailingDot(t *testing.T) {
	statusJSON := []byte(`{
		"Version": "1.96.5-t4ee448d3a-g74ffbefc2",
		"Self": {
			"HostName": "D24WT27C3J",
			"DNSName": "d24wt27c3j.tail113163.ts.net."
		}
	}`)
	got := parseTailscaleDNSName(statusJSON)
	want := "d24wt27c3j.tail113163.ts.net"
	if got != want {
		t.Fatalf("parseTailscaleDNSName = %q, want %q", got, want)
	}
}

// TestParseTailscaleDNSNameEmptyOnMissingField は Self.DNSName が無い（旧版
// tailscale や Self 未設定状態）場合に空文字列を返すことを確認する。
func TestParseTailscaleDNSNameEmptyOnMissingField(t *testing.T) {
	statusJSON := []byte(`{"Version": "1.0.0", "Self": {"HostName": "x"}}`)
	if got := parseTailscaleDNSName(statusJSON); got != "" {
		t.Fatalf("DNSName 未設定なのに %q を返した", got)
	}
}

// TestParseTailscaleDNSNameEmptyOnInvalidJSON は壊れた JSON でパニックせず
// 空文字列を返すことを確認する（tailscale CLI の出力形式が将来変わっても
// この関数呼び出し元（localIPs）を壊さないための保証）。
func TestParseTailscaleDNSNameEmptyOnInvalidJSON(t *testing.T) {
	if got := parseTailscaleDNSName([]byte("not json")); got != "" {
		t.Fatalf("壊れた JSON なのに %q を返した", got)
	}
}

// TestLocalIPsExcludesLoopbackAndLinkLocal は localIPs が実際にこのマシンの
// ネットワークインターフェースから loopback/link-local を除外し、Tailscale の
// CGNAT/IPv6 ULA レンジ含む通常アドレスを返すことを確認する（実 net.InterfaceAddrs
// を使う統合テスト＝環境依存だが、少なくとも loopback が含まれないことは
// どの環境でも成り立つ不変条件）。
func TestLocalIPsExcludesLoopback(t *testing.T) {
	for _, ip := range localIPs() {
		if ip == "127.0.0.1" || ip == "::1" {
			t.Fatalf("localIPs に loopback %q が含まれた", ip)
		}
	}
}
