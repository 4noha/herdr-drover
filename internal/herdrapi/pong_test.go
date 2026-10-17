package herdrapi

import (
	"encoding/json"
	"testing"
)

// 実採取した ping 応答の result。0.9.x で capabilities に整数
// （endpoint_protocol_generation）が混ざり、旧型 map[string]bool では decode
// 自体が失敗して Ping() が常にエラー＝daemon 起動不能になった（実測: "cannot
// unmarshal number into Go struct field PongInfo.capabilities of type bool"）。
func TestPongDecodesAcrossVersions(t *testing.T) {
	for _, tc := range []struct {
		name, raw, capTrue, capFalse string
	}{
		{"0.7.4-macos", `{"type":"pong","version":"0.7.4","protocol":16,` +
			`"capabilities":{"live_handoff":true,"detached_server_daemon":false}}`,
			"live_handoff", "detached_server_daemon"},
		{"0.9.5-windows", `{"type":"pong","version":"0.9.5","protocol":22,` +
			`"capabilities":{"live_handoff":false,"detached_server_daemon":true,` +
			`"endpoint_protocol_generation":1,"surface_interest":true,"health_check":true,` +
			`"ssh_agent_registration":false}}`,
			"detached_server_daemon", "live_handoff"},
	} {
		var p PongInfo
		if err := json.Unmarshal([]byte(tc.raw), &p); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if p.Type != "pong" || p.Version == "" || p.Protocol == 0 {
			t.Errorf("%s: decoded %+v", tc.name, p)
		}
		if !p.Capability(tc.capTrue) {
			t.Errorf("%s: %s should be true", tc.name, tc.capTrue)
		}
		for _, name := range []string{tc.capFalse, "endpoint_protocol_generation", "no_such_capability"} {
			if p.Capability(name) {
				t.Errorf("%s: %s should be false (absent, false or non-bool)", tc.name, name)
			}
		}
	}
}
