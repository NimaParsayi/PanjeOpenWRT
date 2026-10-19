package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseLinks(t *testing.T) {
	vm := base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"vm-ws","add":"a.example.com","port":"443","id":"uuid-1","aid":"0","net":"ws","host":"h.example.com","path":"/p","tls":"tls","sni":"s.example.com"}`))
	ss := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	for _, tc := range []struct {
		link, proto, name string
		port              int
	}{
		{"vmess://" + vm, "vmess", "vm-ws", 443},
		{"vless://uuid@b.example.com:8443?type=grpc&security=reality&sni=x.com&pbk=PUB&sid=ab&serviceName=g#my%20vless", "vless", "my vless", 8443},
		{"trojan://pw@c.example.com:443?sni=c.example.com#tr", "trojan", "tr", 443},
		{"ss://" + ss + "@d.example.com:8388#ss1", "ss", "ss1", 8388},
	} {
		n, out, err := ParseLink(tc.link)
		if err != nil {
			t.Fatalf("%s: %v", tc.proto, err)
		}
		if n.Proto != tc.proto || n.Name != tc.name || n.Port != tc.port || out["protocol"] == nil {
			t.Errorf("%s: got %+v", tc.proto, n)
		}
	}
	// base64 subscription body
	body := base64.StdEncoding.EncodeToString([]byte("trojan://pw@c.example.com:443#a\nss://" + ss + "@d.example.com:8388#b\n"))
	if got := ParseBulk(body); len(got) != 2 {
		t.Errorf("bulk: %d", len(got))
	}
}

func TestBuildXray(t *testing.T) {
	d := defaults()
	for _, l := range []string{"trojan://pw@c.example.com:443#a", "trojan://pw@e.example.com:443#b"} {
		n, _, _ := ParseLink(l)
		n.ID = nodeID(l)
		d.Nodes = append(d.Nodes, n)
		d.Groups[0].Nodes = append(d.Groups[0].Nodes, n.ID)
	}
	d.Groups = append(d.Groups, Group{ID: "rr", Name: "RR", Strategy: "roundRobin", Nodes: d.Groups[0].Nodes})
	d.Rules = append([]Rule{{ID: "x", Enabled: true, Match: "netflix.com\n1.2.3.0/24\nport:8080", Target: "rr"}}, d.Rules...)
	cfg, err := BuildXray(d)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	s := string(b)
	for _, want := range []string{`"balancerTag":"b-rr"`, `"balancerTag":"b-main"`, `"domain:netflix.com"`, `"geoip:ir"`, `"observatory"`, `"mark":255`, `"tproxy"`, `"dns-in"`, `"dns-out"`} {
		if !strings.Contains(s, want) {
			t.Errorf("config missing %s", want)
		}
	}
	d.Groups[0].Nodes = nil
	d.Groups[1].Nodes = nil
	if _, err := BuildXray(d); err == nil {
		t.Error("expected error for empty group")
	}
}
