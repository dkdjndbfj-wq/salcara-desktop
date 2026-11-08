package config

import (
	"path/filepath"
	"testing"
)

func TestCodexAPINetworkValidation(t *testing.T) {
	for _, n := range []CodexAPINetwork{{}, {BindScope: "loopback", Ports: []int{48000}, ClientHost: "localhost"}, {BindScope: "lan", Ports: []int{48000, 48001}, ClientHost: "172.20.1.1"}} {
		if err := ValidateCodexAPINetwork(n); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []CodexAPINetwork{{BindScope: "public"}, {Ports: []int{0}}, {Ports: []int{70000}}, {Ports: []int{48000, 48000}}, {ClientHost: "foreign.example"}, {ClientHost: "8.8.8.8"}, {BindScope: "loopback", ClientHost: "192.168.1.2"}, {BindScope: "lan", ClientHost: "127.0.0.1/evil"}} {
		if ValidateCodexAPINetwork(n) == nil {
			t.Fatalf("accepted invalid network: %+v", n)
		}
	}
	for _,host:=range []string{"::1","fd00::1","127.0.0.2"}{if ValidateCodexAPINetwork(CodexAPINetwork{BindScope:"lan",ClientHost:host})==nil{t.Fatalf("advertised unbound host %s",host)}}
}

func TestCodexAPINetworkChangesFenceAuthorizationAndClone(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Update(func(c *Config) error { c.CodexAPI.Network = CodexAPINetwork{Ports: []int{48000}}; return nil }); err != nil {
		t.Fatal(err)
	}
	a := st.Get()
	epoch := a.CodexAPI.AuthEpoch
	a.CodexAPI.Network.Ports[0] = 48001
	if st.Get().CodexAPI.Network.Ports[0] != 48000 {
		t.Fatal("mutable network snapshot")
	}
	if err = st.Update(func(c *Config) error { c.CodexAPI.Network.BindScope = "lan"; return nil }); err != nil {
		t.Fatal(err)
	}
	if st.Get().CodexAPI.AuthEpoch == epoch {
		t.Fatal("network change kept old authorization")
	}
}

func TestCodexAPIInvalidNetworkNeverServes(t *testing.T) {
	c := Config{CodexAPI: CodexAPIService{Enabled: true}}
	if !c.CodexAPIActive() {
		t.Fatal("legacy default broke")
	}
	c.CodexAPI.Network = CodexAPINetwork{BindScope: "public"}
	if c.CodexAPIActive() {
		t.Fatal("invalid persisted scope remained active")
	}
}
