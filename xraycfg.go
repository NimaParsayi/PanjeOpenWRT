package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"
)

const fwmark = 255
const dnsPort = 5301 // local DNS inbound that LAN port-53 traffic is redirected to

func nodeID(link string) string {
	h := sha256.Sum256([]byte(link))
	return hex.EncodeToString(h[:4])
}

// groupNodes resolves the effective node list of a group (explicit + subscription based).
func groupNodes(d *Data, g Group) []Node {
	byID := map[string]Node{}
	for _, n := range d.Nodes {
		byID[n.ID] = n
	}
	seen := map[string]bool{}
	var out []Node
	add := func(n Node) {
		if !seen[n.ID] {
			seen[n.ID] = true
			out = append(out, n)
		}
	}
	for _, id := range g.Nodes {
		if n, ok := byID[id]; ok {
			add(n)
		}
	}
	for _, sid := range g.Subs {
		for _, n := range d.Nodes {
			if n.Sub == sid {
				add(n)
			}
		}
	}
	return out
}

func splitMatch(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' || r == '\r' || r == '\t' })
}

func isIPEntry(e string) bool {
	if strings.HasPrefix(e, "geoip:") || strings.HasPrefix(e, "ext-ip:") {
		return true
	}
	if _, _, err := net.ParseCIDR(e); err == nil {
		return true
	}
	return net.ParseIP(e) != nil
}

// classify splits user entries into xray domain / ip / port lists.
func classify(match string) (domains, ips []string, port string) {
	var ports []string
	for _, e := range splitMatch(match) {
		switch {
		case strings.HasPrefix(e, "port:"):
			ports = append(ports, strings.TrimPrefix(e, "port:"))
		case isIPEntry(e):
			ips = append(ips, e)
		default:
			if !strings.Contains(e, ":") { // bare "example.com" => domain suffix match
				e = "domain:" + e
			}
			domains = append(domains, e)
		}
	}
	return domains, ips, strings.Join(ports, ",")
}

func BuildXray(d Data) (M, error) {
	s := d.Settings
	groups := map[string]Group{}
	for _, g := range d.Groups {
		groups[g.ID] = g
	}
	usedNodes := map[string]Node{}
	var order []string
	var balancers []M
	needObs := false

	// target -> routing fields
	resolve := func(t string) (M, error) {
		switch t {
		case "direct", "block":
			return M{"outboundTag": t}, nil
		}
		g, ok := groups[t]
		if !ok {
			return nil, fmt.Errorf("unknown target %q", t)
		}
		nodes := groupNodes(&d, g)
		if len(nodes) == 0 {
			return nil, fmt.Errorf("group %q has no nodes", g.Name)
		}
		var tags []string
		for _, n := range nodes {
			if _, ok := usedNodes[n.ID]; !ok {
				usedNodes[n.ID] = n
				order = append(order, n.ID)
			}
			tags = append(tags, "n-"+n.ID)
		}
		if g.Strategy == "single" || len(tags) == 1 {
			return M{"outboundTag": tags[0]}, nil
		}
		st := g.Strategy
		if st == "leastPing" {
			needObs = true
		} else if st != "random" {
			st = "roundRobin"
		}
		bt := "b-" + g.ID
		for _, b := range balancers {
			if b["tag"] == bt {
				return M{"balancerTag": bt}, nil
			}
		}
		balancers = append(balancers, M{"tag": bt, "selector": tags, "strategy": M{"type": st}})
		return M{"balancerTag": bt}, nil
	}

	var rules []M
	for _, r := range d.Rules {
		if !r.Enabled {
			continue
		}
		tgt, err := resolve(r.Target)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.Name, err)
		}
		doms, ips, port := classify(r.Match)
		emit := func(k string, v any) {
			m := M{"type": "field", k: v}
			for a, b := range tgt {
				m[a] = b
			}
			rules = append(rules, m)
		}
		if len(doms) > 0 {
			emit("domain", doms)
		}
		if len(ips) > 0 {
			emit("ip", ips)
		}
		if port != "" {
			emit("port", port)
		}
	}
	dt, err := resolve(s.Default)
	if err != nil {
		if s.Default == "" {
			dt = M{"outboundTag": "direct"}
		} else {
			return nil, fmt.Errorf("default route: %w", err)
		}
	}
	last := M{"type": "field", "network": "tcp,udp"}
	for a, b := range dt {
		last[a] = b
	}
	rules = append(rules, last)

	sock := M{"mark": fwmark}
	outs := []M{
		{"tag": "direct", "protocol": "freedom", "streamSettings": M{"sockopt": sock}},
		{"tag": "block", "protocol": "blackhole"},
	}
	for _, id := range order {
		n := usedNodes[id]
		_, o, err := ParseLink(n.Link)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.Name, err)
		}
		o["tag"] = "n-" + id
		ss, _ := o["streamSettings"].(M)
		if ss == nil {
			ss = M{}
		}
		ss["sockopt"] = M{"mark": fwmark}
		o["streamSettings"] = ss
		outs = append(outs, o)
	}

	bind := "127.0.0.1"
	if s.AllowLan {
		bind = "0.0.0.0"
	}
	sniff := M{"enabled": true, "destOverride": []string{"http", "tls", "quic"}}
	ins := []M{
		{"tag": "socks", "listen": bind, "port": s.SocksPort, "protocol": "socks",
			"settings": M{"udp": true, "auth": "noauth"}, "sniffing": sniff},
		{"tag": "http", "listen": bind, "port": s.HTTPPort, "protocol": "http", "sniffing": sniff},
	}
	if s.Transparent {
		ins = append(ins, M{"tag": "tproxy", "listen": "0.0.0.0", "port": s.TproxyPort, "protocol": "dokodemo-door",
			"settings":       M{"network": "tcp,udp", "followRedirect": true},
			"streamSettings": M{"sockopt": M{"tproxy": "tproxy"}}, "sniffing": sniff})
	}

	if s.Transparent {
		ins = append(ins, M{"tag": "dns-in", "listen": "0.0.0.0", "port": dnsPort, "protocol": "dokodemo-door",
			"settings": M{"address": "1.1.1.1", "port": 53, "network": "tcp,udp"}})
		outs = append(outs, M{"tag": "dns-out", "protocol": "dns"})
		rules = append([]M{{"type": "field", "inboundTag": []string{"dns-in"}, "outboundTag": "dns-out"}}, rules...)
	}
	routing := M{"domainStrategy": "IPIfNonMatch", "rules": rules}
	if len(balancers) > 0 {
		routing["balancers"] = balancers
	}
	dns := M{"queryStrategy": "UseIPv4", "servers": []any{
		M{"address": orStr(s.LocalDNS, "localhost"), "domains": []string{"geosite:category-ir", "domain:ir"}},
		orStr(s.DNS, "https://1.1.1.1/dns-query"),
		"https://8.8.8.8/dns-query",
	}}
	cfg := M{
		"log":       M{"loglevel": orStr(s.LogLevel, "warning")},
		"dns":       dns,
		"inbounds":  ins,
		"outbounds": outs,
		"routing":   routing,
	}
	if needObs {
		cfg["observatory"] = M{"subjectSelector": []string{"n-"},
			"probeUrl": "https://www.gstatic.com/generate_204", "probeInterval": "60s", "enableConcurrency": true}
	}
	return cfg, nil
}
