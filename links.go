package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

type M = map[string]any

func b64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := e.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("bad base64")
}

// ParseLink converts a share link into a Node (display info) and an xray outbound.
func ParseLink(link string) (Node, M, error) {
	link = strings.TrimSpace(link)
	i := strings.Index(link, "://")
	if i < 0 {
		return Node{}, nil, errors.New("not a link")
	}
	scheme := strings.ToLower(link[:i])
	switch scheme {
	case "vmess":
		return parseVmess(link)
	case "vless", "trojan":
		return parseURLStyle(scheme, link)
	case "ss":
		return parseSS(link)
	}
	return Node{}, nil, errors.New("unsupported protocol: " + scheme)
}

func nodeOf(link, proto, name, addr string, port int) Node {
	if name == "" {
		name = addr + ":" + strconv.Itoa(port)
	}
	return Node{Name: name, Link: link, Proto: proto, Addr: addr, Port: port}
}

func parseVmess(link string) (Node, M, error) {
	raw, err := b64(link[len("vmess://"):])
	if err != nil {
		return Node{}, nil, err
	}
	var j map[string]any
	if err := json.Unmarshal(raw, &j); err != nil {
		return Node{}, nil, err
	}
	str := func(k string) string {
		switch v := j[k].(type) {
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	port, _ := strconv.Atoi(str("port"))
	aid, _ := strconv.Atoi(str("aid"))
	scy := str("scy")
	if scy == "" {
		scy = "auto"
	}
	q := url.Values{}
	q.Set("type", str("net"))
	q.Set("security", str("tls"))
	q.Set("sni", str("sni"))
	q.Set("host", str("host"))
	q.Set("path", str("path"))
	q.Set("alpn", str("alpn"))
	q.Set("fp", str("fp"))
	q.Set("headerType", str("type"))
	if str("net") == "grpc" {
		q.Set("serviceName", str("path"))
	}
	out := M{"protocol": "vmess", "settings": M{"vnext": []M{{
		"address": str("add"), "port": port,
		"users": []M{{"id": str("id"), "alterId": aid, "security": scy}},
	}}}, "streamSettings": stream(q)}
	return nodeOf(link, "vmess", str("ps"), str("add"), port), out, nil
}

func parseURLStyle(scheme, link string) (Node, M, error) {
	u, err := url.Parse(link)
	if err != nil {
		return Node{}, nil, err
	}
	port, _ := strconv.Atoi(u.Port())
	if u.Hostname() == "" || port == 0 {
		return Node{}, nil, errors.New("missing host/port")
	}
	q := u.Query()
	user := u.User.Username()
	var out M
	if scheme == "vless" {
		enc := q.Get("encryption")
		if enc == "" {
			enc = "none"
		}
		usr := M{"id": user, "encryption": enc}
		if f := q.Get("flow"); f != "" {
			usr["flow"] = f
		}
		out = M{"protocol": "vless", "settings": M{"vnext": []M{{
			"address": u.Hostname(), "port": port, "users": []M{usr}}}}}
	} else {
		out = M{"protocol": "trojan", "settings": M{"servers": []M{{
			"address": u.Hostname(), "port": port, "password": user}}}}
		if q.Get("security") == "" {
			q.Set("security", "tls")
		}
	}
	out["streamSettings"] = stream(q)
	return nodeOf(link, scheme, u.Fragment, u.Hostname(), port), out, nil
}

func parseSS(link string) (Node, M, error) {
	rest := link[len("ss://"):]
	name := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		name, _ = url.PathUnescape(rest[i+1:])
		rest = rest[:i]
	}
	q := url.Values{}
	if i := strings.Index(rest, "?"); i >= 0 {
		q, _ = url.ParseQuery(rest[i+1:])
		rest = rest[:i]
	}
	rest = strings.TrimSuffix(rest, "/")
	var cred, hostport string
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		cred, hostport = rest[:i], rest[i+1:]
		if d, err := b64(cred); err == nil && strings.Contains(string(d), ":") {
			cred = string(d)
		} else if dec, err := url.PathUnescape(cred); err == nil {
			cred = dec
		}
	} else {
		d, err := b64(rest)
		if err != nil {
			return Node{}, nil, err
		}
		s := string(d)
		j := strings.LastIndex(s, "@")
		if j < 0 {
			return Node{}, nil, errors.New("bad ss link")
		}
		cred, hostport = s[:j], s[j+1:]
	}
	k := strings.Index(cred, ":")
	if k < 0 {
		return Node{}, nil, errors.New("bad ss credentials")
	}
	method, pass := cred[:k], cred[k+1:]
	h := strings.LastIndex(hostport, ":")
	if h < 0 {
		return Node{}, nil, errors.New("bad ss host")
	}
	host := strings.Trim(hostport[:h], "[]")
	port, _ := strconv.Atoi(hostport[h+1:])
	out := M{"protocol": "shadowsocks", "settings": M{"servers": []M{{
		"address": host, "port": port, "method": method, "password": pass}}}}
	if p := q.Get("plugin"); strings.HasPrefix(p, "v2ray-plugin") || strings.HasPrefix(p, "obfs-local") {
		// plugins are not supported by xray directly; keep plain connection
		_ = p
	}
	out["streamSettings"] = M{"network": "tcp"}
	return nodeOf(link, "ss", name, host, port), out, nil
}

// stream builds xray streamSettings from share-link style query params.
func stream(q url.Values) M {
	net := q.Get("type")
	switch net {
	case "", "raw":
		net = "tcp"
	case "h2":
		net = "http"
	}
	s := M{"network": net}
	host, path := q.Get("host"), q.Get("path")
	switch net {
	case "ws":
		w := M{"path": orStr(path, "/")}
		if host != "" {
			w["headers"] = M{"Host": host}
		}
		s["wsSettings"] = w
	case "grpc":
		s["grpcSettings"] = M{"serviceName": orStr(q.Get("serviceName"), path), "multiMode": q.Get("mode") == "multi"}
	case "http":
		h := M{"path": orStr(path, "/")}
		if host != "" {
			h["host"] = strings.Split(host, ",")
		}
		s["httpSettings"] = h
	case "httpupgrade":
		h := M{"path": orStr(path, "/")}
		if host != "" {
			h["host"] = host
		}
		s["httpupgradeSettings"] = h
	case "xhttp", "splithttp":
		s["network"] = "xhttp"
		x := M{"path": orStr(path, "/")}
		if host != "" {
			x["host"] = host
		}
		if m := q.Get("mode"); m != "" {
			x["mode"] = m
		}
		s["xhttpSettings"] = x
	case "kcp":
		s["kcpSettings"] = M{"header": M{"type": orStr(q.Get("headerType"), "none")}, "seed": q.Get("seed")}
	case "tcp":
		if q.Get("headerType") == "http" {
			s["tcpSettings"] = M{"header": M{"type": "http", "request": M{
				"path": strings.Split(orStr(path, "/"), ","), "headers": M{"Host": strings.Split(host, ",")}}}}
		}
	}
	sni := orStr(q.Get("sni"), host)
	var alpn []string
	if a := q.Get("alpn"); a != "" {
		alpn = strings.Split(a, ",")
	}
	insecure := q.Get("allowInsecure") == "1" || q.Get("insecure") == "1"
	switch q.Get("security") {
	case "tls":
		t := M{"allowInsecure": insecure}
		if sni != "" {
			t["serverName"] = sni
		}
		if alpn != nil {
			t["alpn"] = alpn
		}
		if fp := q.Get("fp"); fp != "" {
			t["fingerprint"] = fp
		}
		s["security"] = "tls"
		s["tlsSettings"] = t
	case "reality":
		s["security"] = "reality"
		s["realitySettings"] = M{"serverName": sni, "fingerprint": orStr(q.Get("fp"), "chrome"),
			"publicKey": q.Get("pbk"), "shortId": q.Get("sid"), "spiderX": q.Get("spx")}
	}
	return s
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ParseBulk accepts a subscription body (plain or base64) or pasted links and
// returns all nodes it can understand.
func ParseBulk(body string) []Node {
	body = strings.TrimSpace(body)
	if !strings.Contains(body, "://") {
		if d, err := b64(strings.Join(strings.Fields(body), "")); err == nil {
			body = string(d)
		}
	}
	var out []Node
	for _, ln := range strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if n, _, err := ParseLink(ln); err == nil {
			out = append(out, n)
		}
	}
	return out
}
