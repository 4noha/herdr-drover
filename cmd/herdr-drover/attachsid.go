// attachsid.go — ↗窓の relay sid と、その形式を決める版ゲート。
//
// 従来の派生 sid `<pane_id>#inj` は**見る側の PC を含まない**ため、同じリモート
// セッションに複数 PC が ↗窓を持つと relay の viewer 枠（sid につき 1 つ）を
// 奪い合い、約 32 秒ごと（奪われた側の backoff 上限 30s）に相互に切断し続けた
// （BUGS.md BUG-16・2026-11-03 relay ログで 4 IP が交互に接続するのを確認）。
//
// v0.5.48 からは `<pane_id>#inj@<viewer pc>` で PC ごとに枠を分ける。source 側
// （webterm.injPaneID）が新形式を解釈できるのは v0.5.48 以降なので、リモートの
// 版（pcs/{pc}.cm_version）を見て形式を選ぶ。版が読めない／古い／dev なら旧形式
// ＝旧 PC が相手でも ↗窓が空にならない。
package main

import (
	"regexp"
	"strconv"
)

// injSuffix は派生 sid の印。viewer PC 付きは injSuffix + "@" + pc。
const injSuffix = "#inj"

// viewerSidMinVersion は新形式を解釈できる source の最小版。release 前の
// `git describe` 形（v0.5.47-N-g…・N≥1）も新しい側に倒すため、4 つ目に
// コミット数を持つ（v0.5.47 そのものは 0 ＝旧）。
var viewerSidMinVersion = [4]int{0, 5, 47, 1}

var reVersion = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-(\d+)-g[0-9a-f]+)?(?:-dirty)?$`)

// parseVersion は "v0.5.47" / "v0.5.47-7-g03b48f9(-dirty)" を数に分解する。
// 形が違う（"dev"・空）は ok=false。
func parseVersion(v string) (out [4]int, ok bool) {
	m := reVersion.FindStringSubmatch(v)
	if m == nil {
		return out, false
	}
	for i := 0; i < 4; i++ {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// sourceSupportsViewerSid は source の版が `#inj@<pc>` 形式を解釈できるか。
func sourceSupportsViewerSid(version string) bool {
	v, ok := parseVersion(version)
	if !ok {
		return false
	}
	for i := 0; i < 4; i++ {
		if v[i] != viewerSidMinVersion[i] {
			return v[i] > viewerSidMinVersion[i]
		}
	}
	return true
}

// injSidFor は relay に使う sid。viewerPC が空なら旧形式。
func injSidFor(sid, viewerPC, sourceVersion string) string {
	if viewerPC != "" && sourceSupportsViewerSid(sourceVersion) {
		return sid + injSuffix + "@" + viewerPC
	}
	return sid + injSuffix
}
