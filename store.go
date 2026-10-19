package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Node struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Link  string `json:"link"`
	Proto string `json:"proto"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
	Sub   string `json:"sub,omitempty"` // owning subscription id
	Ping  int    `json:"ping,omitempty"`
}

type Sub struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Hours   int    `json:"hours"` // auto-update interval, 0 = manual
	Updated int64  `json:"updated"`
	Count   int    `json:"count"`
	LastErr string `json:"lastErr,omitempty"`
}

// Group picks outbound node(s) for a rule target.
// Strategy: single | roundRobin | random | leastPing
// Nodes may list node ids; Subs lists subscription ids whose nodes are included dynamically.
type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Strategy string   `json:"strategy"`
	Nodes    []string `json:"nodes"`
	Subs     []string `json:"subs,omitempty"`
}

// Rule matches traffic; Match is a newline/comma separated list of xray style
// entries (geosite:xx, geoip:xx, domain:x, full:x, keyword:x, regexp:x, CIDR, port:80,443).
// Target: direct | block | <group id>.
type Rule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Match   string `json:"match"`
	Target  string `json:"target"`
}

type Settings struct {
	Listen      string `json:"listen"`      // panel listen address
	Transparent bool   `json:"transparent"` // tproxy for LAN clients
	LanIface    string `json:"lanIface"`
	TproxyPort  int    `json:"tproxyPort"`
	SocksPort   int    `json:"socksPort"`
	HTTPPort    int    `json:"httpPort"`
	AllowLan    bool   `json:"allowLan"` // bind socks/http on 0.0.0.0
	Default     string `json:"default"`  // target for unmatched traffic
	DNS         string `json:"dns"`      // remote DNS (via proxy)
	LocalDNS    string `json:"localDns"` // DNS for direct/Iran domains
	LogLevel    string `json:"logLevel"`
	XrayBin     string `json:"xrayBin"`
	AssetDir    string `json:"assetDir"`
	GeoIPURL    string `json:"geoipUrl"`
	GeoSiteURL  string `json:"geositeUrl"`
	AutoStart   bool   `json:"autoStart"`
	Enabled     bool   `json:"enabled"` // desired running state
}

type Data struct {
	Settings Settings `json:"settings"`
	Nodes    []Node   `json:"nodes"`
	Subs     []Sub    `json:"subs"`
	Groups   []Group  `json:"groups"`
	Rules    []Rule   `json:"rules"`
	PassSalt string   `json:"passSalt"`
	PassHash string   `json:"passHash"`
}

type Store struct {
	mu   sync.RWMutex
	path string
	D    Data
}

func rid() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashPass(salt, pw string) string {
	k, _ := pbkdf2.Key(sha256.New, pw, []byte(salt), 100000, 32)
	return hex.EncodeToString(k)
}

func defaults() Data {
	return Data{
		Settings: Settings{
			Listen: "0.0.0.0:9090", Transparent: true, LanIface: "br-lan",
			TproxyPort: 12345, SocksPort: 1080, HTTPPort: 1081,
			Default: "main", DNS: "https://1.1.1.1/dns-query", LocalDNS: "localhost",
			LogLevel: "warning", XrayBin: "", AssetDir: "/usr/share/xray",
			GeoIPURL:   "https://github.com/Chocolate4U/Iran-v2ray-rules/releases/latest/download/geoip.dat",
			GeoSiteURL: "https://github.com/Chocolate4U/Iran-v2ray-rules/releases/latest/download/geosite.dat",
			AutoStart:  true,
		},
		Groups: []Group{{ID: "main", Name: "Main", Strategy: "leastPing"}},
		Rules: []Rule{
			{ID: "r-ir", Name: "Iran direct", Enabled: true, Target: "direct",
				Match: "geosite:category-ir\ngeoip:ir\ndomain:ir"},
			{ID: "r-lan", Name: "Private IPs", Enabled: true, Target: "direct", Match: "geoip:private"},
			{ID: "r-ads", Name: "Block ads", Enabled: false, Target: "block", Match: "geosite:category-ads-all"},
		},
	}
}

func OpenStore(path string) (*Store, string, error) {
	s := &Store{path: path, D: defaults()}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s.D); err != nil {
			return nil, "", err
		}
	} else if !os.IsNotExist(err) {
		return nil, "", err
	}
	initial := ""
	if s.D.PassHash == "" {
		initial = rid() + rid()
		s.setPass(initial)
		if err := s.Save(); err != nil {
			return nil, "", err
		}
	}
	return s, initial, nil
}

func (s *Store) setPass(pw string) {
	s.D.PassSalt = rid() + rid()
	s.D.PassHash = hashPass(s.D.PassSalt, pw)
}

func (s *Store) CheckPass(pw string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return hashPass(s.D.PassSalt, pw) == s.D.PassHash
}

// Save must be called with the write lock held or before sharing.
func (s *Store) Save() error {
	b, _ := json.MarshalIndent(&s.D, "", " ")
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Update runs fn under the write lock and persists.
func (s *Store) Update(fn func(d *Data)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.D)
	return s.Save()
}

// Snapshot returns a deep copy safe to use without locks.
func (s *Store) Snapshot() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(&s.D)
	var d Data
	json.Unmarshal(b, &d)
	// nil slices would serialize as null and break the UI
	if d.Nodes == nil {
		d.Nodes = []Node{}
	}
	if d.Subs == nil {
		d.Subs = []Sub{}
	}
	if d.Groups == nil {
		d.Groups = []Group{}
	}
	if d.Rules == nil {
		d.Rules = []Rule{}
	}
	return d
}

var _ = time.Now
