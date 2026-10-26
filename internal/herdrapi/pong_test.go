package herdrapi

import "testing"

// TestDecodePongAcrossHerdrVersions は ping 応答を herdr の版をまたいで読めることを
// 固定する。
//
// ⚠これが落ちると herdr 0.9.x で drover が起動しない（BUG-12）:
// 0.9.5 は capabilities に **bool 以外の値**（endpoint_protocol_generation=1 の
// 数値）を混ぜて返すため、旧実装の `map[string]bool` は
// `cannot unmarshal number into Go struct field PongInfo.capabilities of type bool`
// で decode に失敗し、daemon は起動直後に exit 1（launchd が 100 回超 respawn）、
// `claude` シムも「herdr server が 10s 以内に応答しない」と誤診して使えなくなった。
//
// 下の JSON は 2026-10-25 に実 herdr 0.9.5 の socket を直叩きして採取した原文。
func TestDecodePongAcrossHerdrVersions(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		version string
		proto   int
		wantCap map[string]bool // bool 値だけを期待（非 bool は Capabilities に載らなくてよい）
	}{
		{
			name: "0.7.4 (全部 bool)",
			raw: `{"type":"pong","version":"0.7.4","protocol":16,` +
				`"capabilities":{"live_handoff":true,"detached_server_daemon":false}}`,
			version: "0.7.4", proto: 16,
			wantCap: map[string]bool{"live_handoff": true, "detached_server_daemon": false},
		},
		{
			name: "0.9.5 (数値が混ざる・実採取)",
			raw: `{"type":"pong","version":"0.9.5","protocol":22,` +
				`"capabilities":{"live_handoff":true,"detached_server_daemon":true,` +
				`"endpoint_protocol_generation":1,"surface_interest":true,"health_check":true,` +
				`"ssh_agent_registration":true}}`,
			version: "0.9.5", proto: 22,
			wantCap: map[string]bool{"live_handoff": true, "detached_server_daemon": true,
				"surface_interest": true, "health_check": true, "ssh_agent_registration": true},
		},
		{
			name:    "capabilities 無し",
			raw:     `{"type":"pong","version":"0.9.9","protocol":30}`,
			version: "0.9.9", proto: 30,
			wantCap: map[string]bool{},
		},
		{
			name:    "capabilities が object 以外（将来の形）でも落ちない",
			raw:     `{"type":"pong","version":"1.0.0","protocol":40,"capabilities":["a","b"]}`,
			version: "1.0.0", proto: 40,
			wantCap: map[string]bool{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pong, err := decodePong([]byte(tc.raw))
			if err != nil {
				t.Fatalf("ping 応答が decode できない: %v\n原文: %s", err, tc.raw)
			}
			if pong.Type != "pong" || pong.Version != tc.version || pong.Protocol != tc.proto {
				t.Fatalf("基本フィールドが違う: %+v", pong)
			}
			for k, want := range tc.wantCap {
				if got, ok := pong.Capabilities[k]; !ok || got != want {
					t.Fatalf("capabilities[%q]=%v(ok=%v) want %v", k, got, ok, want)
				}
			}
			// 非 bool の値は捨てるが、bool として捏造してはならない。
			if _, ok := pong.Capabilities["endpoint_protocol_generation"]; ok {
				t.Fatalf("数値の capability を bool として載せている（捏造）: %v", pong.Capabilities)
			}
		})
	}
}
