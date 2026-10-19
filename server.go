package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html web/vazirmatn.woff2 web/flags.woff2 web/logo.png web/icon.png web/icon-192.png web/icon-512.png web/icon-maskable.png web/apple-touch-icon.png web/manifest.webmanifest web/sw.js
var webFS embed.FS

type Server struct {
	st   *Store
	core *Core

	mu       sync.Mutex
	sessions map[string]time.Time
	fails    map[string][]time.Time
}

func NewServer(st *Store, c *Core) *Server {
	return &Server{st: st, core: c, sessions: map[string]time.Time{}, fails: map[string][]time.Time{}}
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	jsonOut(w, code, M{"error": err.Error()})
}

func (s *Server) authed(r *http.Request) bool {
	ck, err := r.Cookie("panje")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[ck.Value]
	if !ok || time.Now().After(exp) {
		delete(s.sessions, ck.Value)
		return false
	}
	return true
}

func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authed(r) {
			fail(w, 401, errors.New("unauthorized"))
			return
		}
		if r.Method != "GET" && r.Header.Get("X-Panje") == "" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			fail(w, 415, errors.New("json required")) // blocks simple cross-site form posts
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		h(w, r)
	}
}

func decode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("web/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	})
	for name, ctype := range map[string]string{"vazirmatn.woff2": "font/woff2", "flags.woff2": "font/woff2",
		"logo.png": "image/png", "icon.png": "image/png", "icon-192.png": "image/png", "icon-512.png": "image/png",
		"icon-maskable.png": "image/png", "apple-touch-icon.png": "image/png",
		"manifest.webmanifest": "application/manifest+json", "sw.js": "text/javascript"} {
		b, _ := webFS.ReadFile("web/" + name)
		mux.HandleFunc("GET /"+name, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ctype)
			if name == "sw.js" {
				w.Header().Set("Cache-Control", "no-cache")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=86400")
			}
			w.Write(b)
		})
	}
	mux.HandleFunc("GET /api/dat", s.guard(s.listDat))
	mux.HandleFunc("POST /api/dat/{name}", s.guard(s.putDat))
	mux.HandleFunc("DELETE /api/dat/{name}", s.guard(s.delDat))
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.guard(func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("panje"); err == nil {
			s.mu.Lock()
			delete(s.sessions, ck.Value)
			s.mu.Unlock()
		}
		jsonOut(w, 200, M{"ok": true})
	}))
	mux.HandleFunc("GET /api/state", s.guard(s.state))
	mux.HandleFunc("PUT /api/settings", s.guard(s.putSettings))
	mux.HandleFunc("POST /api/nodes/import", s.guard(s.importNodes))
	mux.HandleFunc("DELETE /api/nodes/{id}", s.guard(s.delNode))
	mux.HandleFunc("POST /api/nodes/ping", s.guard(func(w http.ResponseWriter, r *http.Request) {
		s.core.PingAll()
		s.state(w, r)
	}))
	mux.HandleFunc("POST /api/subs", s.guard(s.addSub))
	mux.HandleFunc("POST /api/subs/{id}/update", s.guard(s.updSub))
	mux.HandleFunc("DELETE /api/subs/{id}", s.guard(s.delSub))
	mux.HandleFunc("PUT /api/groups", s.guard(s.putGroups))
	mux.HandleFunc("PUT /api/rules", s.guard(s.putRules))
	mux.HandleFunc("POST /api/rules/quick", s.guard(s.quickRule))
	mux.HandleFunc("POST /api/apply", s.guard(func(w http.ResponseWriter, r *http.Request) {
		s.st.Update(func(d *Data) { d.Settings.Enabled = true })
		s.reply(w, r, s.core.Apply())
	}))
	mux.HandleFunc("POST /api/stop", s.guard(func(w http.ResponseWriter, r *http.Request) {
		s.st.Update(func(d *Data) { d.Settings.Enabled = false })
		s.core.Stop()
		s.reply(w, r, nil)
	}))
	mux.HandleFunc("POST /api/geo", s.guard(func(w http.ResponseWriter, r *http.Request) {
		err := s.core.UpdateGeo()
		if err == nil && s.core.Status().Running {
			err = s.core.Apply()
		}
		s.reply(w, r, err)
	}))
	mux.HandleFunc("GET /api/logs", s.guard(func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, s.core.Logs())
	}))
	mux.HandleFunc("POST /api/password", s.guard(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Password string `json:"password"`
		}
		if decode(r, &b) != nil || len(b.Password) < 6 {
			fail(w, 400, errors.New("password must be at least 6 characters"))
			return
		}
		s.st.Update(func(d *Data) {
			d.PassSalt = rid() + rid()
			d.PassHash = hashPass(d.PassSalt, b.Password)
		})
		s.mu.Lock()
		s.sessions = map[string]time.Time{}
		s.mu.Unlock()
		jsonOut(w, 200, M{"ok": true})
	}))
	return mux
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	recent := s.fails[ip][:0]
	for _, t := range s.fails[ip] {
		if time.Since(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	s.fails[ip] = recent
	locked := len(recent) >= 5
	s.mu.Unlock()
	if locked {
		fail(w, 429, errors.New("too many attempts, wait a minute"))
		return
	}
	var b struct {
		Password string `json:"password"`
	}
	if decode(r, &b) != nil || !s.st.CheckPass(b.Password) {
		s.mu.Lock()
		s.fails[ip] = append(s.fails[ip], time.Now())
		s.mu.Unlock()
		time.Sleep(700 * time.Millisecond)
		fail(w, 401, errors.New("wrong password"))
		return
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	tok := hex.EncodeToString(raw)
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(30 * 24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "panje", Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 30 * 86400})
	jsonOut(w, 200, M{"ok": true})
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	d := s.st.Snapshot()
	d.PassHash, d.PassSalt = "", ""
	jsonOut(w, 200, M{"data": d, "status": s.core.Status()})
}

// reply returns fresh state, or the error alongside it.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, err error) {
	d := s.st.Snapshot()
	d.PassHash, d.PassSalt = "", ""
	out := M{"data": d, "status": s.core.Status()}
	if err != nil {
		out["error"] = err.Error()
	}
	jsonOut(w, 200, out)
}

// reapply restarts the core after a config change if the user has it enabled.
func (s *Server) reapply(w http.ResponseWriter, r *http.Request) {
	var err error
	if s.st.Snapshot().Settings.Enabled {
		err = s.core.Apply()
	}
	s.reply(w, r, err)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var in Settings
	if err := decode(r, &in); err != nil {
		fail(w, 400, err)
		return
	}
	for _, p := range []int{in.TproxyPort, in.SocksPort, in.HTTPPort} {
		if p < 1 || p > 65535 {
			fail(w, 400, errors.New("invalid port"))
			return
		}
	}
	s.st.Update(func(d *Data) {
		in.Enabled = d.Settings.Enabled
		d.Settings = in
	})
	s.reapply(w, r)
}

func (s *Server) importNodes(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Text string `json:"text"`
	}
	if decode(r, &b) != nil {
		fail(w, 400, errors.New("bad request"))
		return
	}
	nodes := ParseBulk(b.Text)
	if len(nodes) == 0 {
		fail(w, 400, errors.New("no valid vmess/vless/trojan/ss links found"))
		return
	}
	s.st.Update(func(d *Data) {
		have := map[string]bool{}
		for _, n := range d.Nodes {
			have[n.ID] = true
		}
		for _, n := range nodes {
			n.ID = nodeID(n.Link)
			if !have[n.ID] {
				have[n.ID] = true
				d.Nodes = append(d.Nodes, n)
			}
		}
	})
	s.reply(w, r, nil)
}

func (s *Server) delNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.st.Update(func(d *Data) {
		d.Nodes = filter(d.Nodes, func(n Node) bool { return n.ID != id })
		for i := range d.Groups {
			d.Groups[i].Nodes = filter(d.Groups[i].Nodes, func(x string) bool { return x != id })
		}
	})
	s.reapply(w, r)
}

func filter[T any](in []T, keep func(T) bool) []T {
	out := in[:0]
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) addSub(w http.ResponseWriter, r *http.Request) {
	var b Sub
	if decode(r, &b) != nil || !strings.HasPrefix(b.URL, "http") {
		fail(w, 400, errors.New("a valid http(s) URL is required"))
		return
	}
	b.ID = rid()
	if b.Name == "" {
		b.Name = "Subscription"
	}
	s.st.Update(func(d *Data) { d.Subs = append(d.Subs, b) })
	changed, err := s.core.UpdateSub(b.ID)
	if err == nil && changed {
		s.reapply(w, r)
		return
	}
	s.reply(w, r, err)
}

func (s *Server) updSub(w http.ResponseWriter, r *http.Request) {
	changed, err := s.core.UpdateSub(r.PathValue("id"))
	if err == nil && changed {
		s.reapply(w, r)
		return
	}
	s.reply(w, r, err)
}

func (s *Server) delSub(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.st.Update(func(d *Data) {
		d.Subs = filter(d.Subs, func(x Sub) bool { return x.ID != id })
		d.Nodes = filter(d.Nodes, func(n Node) bool { return n.Sub != id })
		for i := range d.Groups {
			d.Groups[i].Subs = filter(d.Groups[i].Subs, func(x string) bool { return x != id })
		}
	})
	s.reapply(w, r)
}

func (s *Server) putGroups(w http.ResponseWriter, r *http.Request) {
	var in []Group
	if decode(r, &in) != nil {
		fail(w, 400, errors.New("bad request"))
		return
	}
	for i := range in {
		if in[i].ID == "" {
			in[i].ID = rid()
		}
	}
	s.st.Update(func(d *Data) {
		d.Groups = in
		ids := map[string]bool{"direct": true, "block": true}
		for _, g := range in {
			ids[g.ID] = true
		}
		for i := range d.Rules {
			if !ids[d.Rules[i].Target] {
				d.Rules[i].Target = "direct"
			}
		}
		if !ids[d.Settings.Default] {
			d.Settings.Default = "direct"
		}
	})
	s.reapply(w, r)
}

func (s *Server) putRules(w http.ResponseWriter, r *http.Request) {
	var in []Rule
	if decode(r, &in) != nil {
		fail(w, 400, errors.New("bad request"))
		return
	}
	for i := range in {
		if in[i].ID == "" {
			in[i].ID = rid()
		}
	}
	s.st.Update(func(d *Data) { d.Rules = in })
	s.reapply(w, r)
}

// quickRule adds a rule at the top: {"match":"netflix.com\nyoutube.com","target":"<group>"}
func (s *Server) quickRule(w http.ResponseWriter, r *http.Request) {
	var in Rule
	if decode(r, &in) != nil || strings.TrimSpace(in.Match) == "" {
		fail(w, 400, errors.New("enter at least one domain / IP"))
		return
	}
	in.ID, in.Enabled = rid(), true
	if in.Target == "" {
		in.Target = "direct"
	}
	if in.Name == "" {
		in.Name = splitMatch(in.Match)[0]
	}
	s.st.Update(func(d *Data) { d.Rules = append([]Rule{in}, d.Rules...) })
	s.reapply(w, r)
}

var datName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}\.dat$`)

func (s *Server) assetDir() string { return s.st.Snapshot().Settings.AssetDir }

func (s *Server) listDat(w http.ResponseWriter, r *http.Request) {
	type F struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Mod  int64  `json:"mod"`
	}
	out := []F{}
	ents, _ := os.ReadDir(s.assetDir())
	for _, e := range ents {
		if fi, err := e.Info(); err == nil && !e.IsDir() && datName.MatchString(e.Name()) {
			out = append(out, F{e.Name(), fi.Size(), fi.ModTime().Unix()})
		}
	}
	jsonOut(w, 200, out)
}

// putDat stores a raw .dat upload (geoip.dat, geosite.dat or custom lists used as ext:file.dat:tag).
func (s *Server) putDat(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !datName.MatchString(name) {
		fail(w, 400, errors.New("file name must look like name.dat"))
		return
	}
	dir := s.assetDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, 500, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
	tmp := filepath.Join(dir, name+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		fail(w, 500, err)
		return
	}
	n, err := io.Copy(f, r.Body)
	f.Close()
	if err != nil || n < 16 {
		os.Remove(tmp)
		fail(w, 400, errors.New("upload failed or file empty"))
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		fail(w, 500, err)
		return
	}
	s.core.logf("uploaded %s (%d KB)", name, n/1024)
	s.reapply(w, r)
}

func (s *Server) delDat(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !datName.MatchString(name) {
		fail(w, 400, errors.New("bad name"))
		return
	}
	os.Remove(filepath.Join(s.assetDir(), name))
	jsonOut(w, 200, M{"ok": true})
}
