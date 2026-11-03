// relayurl.go — WS 中継（relay /session）の接続先を実行中に切り替える。
//
// 接続先は従来 CLOUD_RELAY_URL / config.json（cm 由来・Cloud Run の URL）だけだった。
// Cloud Run は WS 常時接続で 1 vCPU が 24 時間課金されるため、WS の中継だけを VM へ移す
// （drover-cloud v0.2.7・state.SetRelayURL）。切替は Firestore の settings/relay 1 か所:
//   - master: state.WatchRelayURL で real-time に追う（agent.go）
//   - slave : /slave/revoked の応答の ws_url で追う（relaystate.go・Firestore を直接読めない）
//   - ↗窓 attach: 接続のたびに state.RelayURL を読む（attach.go）
//
// 空なら設定の URL（＝戻すのは url を空にするだけ）。HTTP の API（/slave/*・Web）は
// 従来の URL のまま＝ここで切り替えるのはデータ線（WS）だけ。
package main

import (
	"log"
	"strings"
	"sync"
)

// validRelayURL は ws:// / wss:// だけを認める（他はログして無視＝黙って壊さない）。
func validRelayURL(u string) bool {
	return strings.HasPrefix(u, "wss://") || strings.HasPrefix(u, "ws://")
}

// relayURLSource は「設定の URL」と「切替で来た URL」を持ち、get で現在の接続先を返す。
type relayURLSource struct {
	mu       sync.Mutex
	fallback string
	override string
	lg       *log.Logger
}

func newRelayURLSource(fallback string, lg *log.Logger) *relayURLSource {
	return &relayURLSource{fallback: fallback, lg: lg}
}

func (s *relayURLSource) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.override != "" {
		return s.override
	}
	return s.fallback
}

// set は切替で来た URL を取り込む（空＝解除）。接続先が変わった時だけログする。
func (s *relayURLSource) set(url string) {
	if url != "" && !validRelayURL(url) {
		if s.lg != nil {
			s.lg.Printf("relay URL の切替値が不正（無視）: %q", url)
		}
		return
	}
	s.mu.Lock()
	before := s.override
	if before == "" {
		before = s.fallback
	}
	s.override = url
	after := s.override
	if after == "" {
		after = s.fallback
	}
	s.mu.Unlock()
	if before != after && s.lg != nil {
		s.lg.Printf("relay URL 切替: %s → %s（settings/relay）", before, after)
	}
}
