package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var runDir = "/var/run/panje"

type Core struct {
	st      *Store
	mu      sync.Mutex
	cmd     *exec.Cmd
	gen     int
	running bool
	lastErr string
	since   time.Time

	logMu sync.Mutex
	logs  []string
}

func NewCore(st *Store) *Core { return &Core{st: st} }

func (c *Core) logf(f string, a ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(f, a...)
	c.logMu.Lock()
	c.logs = append(c.logs, line)
	if len(c.logs) > 400 {
		c.logs = c.logs[len(c.logs)-300:]
	}
	c.logMu.Unlock()
	fmt.Println(line)
}

func (c *Core) Logs() []string {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	return append([]string(nil), c.logs...)
}

type Status struct {
	Running bool   `json:"running"`
	Since   int64  `json:"since"`
	Err     string `json:"err"`
}

func (c *Core) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{c.running, c.since.Unix(), c.lastErr}
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Apply regenerates config and (re)starts xray plus firewall rules.
func (c *Core) Apply() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.apply()
	if err != nil {
		c.lastErr = err.Error()
		c.logf("apply failed: %v", err)
	} else {
		c.lastErr = ""
	}
	return err
}

func (c *Core) apply() error {
	d := c.st.Snapshot()
	cfg, err := BuildXray(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		runDir = filepath.Join(os.TempDir(), "panje")
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			return err
		}
	}
	path := filepath.Join(runDir, "xray.json")
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	s := d.Settings
	env := append(os.Environ(), "XRAY_LOCATION_ASSET="+s.AssetDir)
	bin, pre := xrayCmd(s)
	test := exec.Command(bin, append(pre, "-test", "-c", path)...)
	test.Env = env
	if out, err := test.CombinedOutput(); err != nil {
		return fmt.Errorf("config rejected by xray: %s", lastLines(string(out), 6))
	}
	c.stopLocked()
	cmd := exec.Command(bin, append(pre, "-c", path)...)
	cmd.Env = env
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			c.logf("xray: %s", sc.Text())
		}
	}()
	c.gen++
	gen := c.gen
	c.cmd, c.running, c.since = cmd, true, time.Now()
	go func() {
		err := cmd.Wait()
		pw.Close()
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.gen != gen {
			return // replaced or stopped intentionally
		}
		c.running = false
		c.removeFirewall()
		c.lastErr = fmt.Sprintf("xray exited: %v", err)
		c.logf("%s", c.lastErr)
		go func() { // crash recovery
			time.Sleep(5 * time.Second)
			want := c.st.Snapshot().Settings.Enabled
			c.mu.Lock()
			want = want && c.gen == gen && !c.running
			c.mu.Unlock()
			if want {
				c.Apply()
			}
		}()
	}()
	if s.Transparent {
		if err := c.setupFirewall(s); err != nil {
			c.stopLocked()
			return err
		}
	}
	c.logf("started (pid %d)", cmd.Process.Pid)
	return nil
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

func (c *Core) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	c.logf("stopped")
}

func (c *Core) stopLocked() {
	c.gen++
	c.removeFirewall()
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd = nil
	}
	c.running = false
}

func (c *Core) setupFirewall(s Settings) error {
	c.removeFirewall()
	port := strconv.Itoa(s.TproxyPort)
	script := fmt.Sprintf(`table inet panje {
 chain dns {
  type nat hook prerouting priority dstnat; policy accept;
  iifname "%[1]s" udp dport 53 redirect to :%[3]d
  iifname "%[1]s" tcp dport 53 redirect to :%[3]d
 }
 chain prerouting {
  type filter hook prerouting priority mangle; policy accept;
  iifname != "%[1]s" return
  fib daddr type { local, broadcast, multicast } return
  ip daddr { 0.0.0.0/8, 10.0.0.0/8, 100.64.0.0/10, 127.0.0.0/8, 169.254.0.0/16, 172.16.0.0/12, 192.168.0.0/16, 224.0.0.0/4, 240.0.0.0/4 } return
  meta nfproto ipv4 meta l4proto tcp meta mark set 1 tproxy ip to :%[2]s accept
  meta nfproto ipv4 meta l4proto udp meta mark set 1 tproxy ip to :%[2]s accept
 }
}
`, s.LanIface, port, dnsPort)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, strings.TrimSpace(string(out)))
	}
	_ = run("ip", "rule", "add", "fwmark", "1", "lookup", "100")
	_ = run("ip", "route", "add", "local", "0.0.0.0/0", "dev", "lo", "table", "100")
	return nil
}

func (c *Core) removeFirewall() {
	exec.Command("nft", "delete", "table", "inet", "panje").Run()
	exec.Command("ip", "rule", "del", "fwmark", "1", "lookup", "100").Run()
	exec.Command("ip", "route", "del", "local", "0.0.0.0/0", "dev", "lo", "table", "100").Run()
}

// ---------- fetching (subscriptions, geo data) ----------

// fetch tries a direct download first and falls back to the local HTTP proxy.
func (c *Core) fetch(raw string) ([]byte, error) {
	get := func(proxy string) ([]byte, error) {
		tr := &http.Transport{}
		if proxy != "" {
			u, _ := url.Parse(proxy)
			tr.Proxy = http.ProxyURL(u)
		}
		cl := &http.Client{Transport: tr, Timeout: 40 * time.Second}
		req, _ := http.NewRequest("GET", raw, nil)
		req.Header.Set("User-Agent", "v2rayN/6.45")
		resp, err := cl.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, errors.New(resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	}
	b, err := get("")
	if err == nil {
		return b, nil
	}
	if c.Status().Running {
		port := c.st.Snapshot().Settings.HTTPPort
		if b2, err2 := get("http://127.0.0.1:" + strconv.Itoa(port)); err2 == nil {
			return b2, nil
		}
	}
	return nil, err
}

// UpdateSub downloads a subscription and swaps its nodes. Returns whether nodes changed.
func (c *Core) UpdateSub(id string) (bool, error) {
	d := c.st.Snapshot()
	var sub *Sub
	for i := range d.Subs {
		if d.Subs[i].ID == id {
			sub = &d.Subs[i]
		}
	}
	if sub == nil {
		return false, errors.New("unknown subscription")
	}
	body, err := c.fetch(sub.URL)
	var nodes []Node
	if err == nil {
		nodes = ParseBulk(string(body))
		if len(nodes) == 0 {
			err = errors.New("no supported nodes found")
		}
	}
	changed := false
	uerr := c.st.Update(func(d *Data) {
		for i := range d.Subs {
			if d.Subs[i].ID != id {
				continue
			}
			if err != nil {
				d.Subs[i].LastErr = err.Error()
				return
			}
			old := map[string]Node{}
			var keep []Node
			for _, n := range d.Nodes {
				if n.Sub == id {
					old[n.ID] = n
				} else {
					keep = append(keep, n)
				}
			}
			seen := map[string]bool{}
			for _, n := range nodes {
				n.ID = nodeID(n.Link)
				if seen[n.ID] {
					continue
				}
				seen[n.ID] = true
				n.Sub = id
				if o, ok := old[n.ID]; ok {
					n.Ping = o.Ping
				} else {
					changed = true
				}
				keep = append(keep, n)
			}
			if len(old) != len(seen) {
				changed = true
			}
			d.Nodes = keep
			d.Subs[i].Updated = time.Now().Unix()
			d.Subs[i].Count = len(seen)
			d.Subs[i].LastErr = ""
		}
	})
	if err != nil {
		return false, err
	}
	return changed, uerr
}

func (c *Core) UpdateGeo() error {
	s := c.st.Snapshot().Settings
	_ = os.MkdirAll(s.AssetDir, 0o755)
	for name, u := range map[string]string{"geoip.dat": s.GeoIPURL, "geosite.dat": s.GeoSiteURL} {
		b, err := c.fetch(u)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if len(b) < 1024 {
			return fmt.Errorf("%s: file too small", name)
		}
		tmp := filepath.Join(s.AssetDir, name+".tmp")
		if err := os.WriteFile(tmp, b, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(s.AssetDir, name)); err != nil {
			return err
		}
		c.logf("updated %s (%d KB)", name, len(b)/1024)
	}
	return nil
}

// PingAll measures TCP connect latency to every node (-1 = unreachable).
func (c *Core) PingAll() {
	d := c.st.Snapshot()
	res := make([]int, len(d.Nodes))
	sem := make(chan struct{}, 24)
	var wg sync.WaitGroup
	for i, n := range d.Nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			t := time.Now()
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(n.Addr, strconv.Itoa(n.Port)))
			if err != nil {
				res[i] = -1
				return
			}
			conn.Close()
			res[i] = int(time.Since(t).Milliseconds()) + 1
		}()
	}
	wg.Wait()
	m := map[string]int{}
	for i, n := range d.Nodes {
		m[n.ID] = res[i]
	}
	c.st.Update(func(d *Data) {
		for i := range d.Nodes {
			if v, ok := m[d.Nodes[i].ID]; ok {
				d.Nodes[i].Ping = v
			}
		}
	})
}

// Background keeps subscriptions fresh.
func (c *Core) Background() {
	for range time.Tick(10 * time.Minute) {
		d := c.st.Snapshot()
		changed := false
		for _, s := range d.Subs {
			if s.Hours > 0 && time.Since(time.Unix(s.Updated, 0)) > time.Duration(s.Hours)*time.Hour {
				if ch, err := c.UpdateSub(s.ID); err != nil {
					c.logf("sub %s: %v", s.Name, err)
				} else if ch {
					changed = true
				}
			}
		}
		if changed && c.Status().Running {
			c.Apply()
		}
	}
}

// xrayCmd returns the engine to launch: the built-in one unless an external
// xray binary is configured and present.
func xrayCmd(s Settings) (string, []string) {
	if s.XrayBin != "" {
		if _, err := os.Stat(s.XrayBin); err == nil {
			return s.XrayBin, []string{"run"}
		}
	}
	self, _ := os.Executable()
	return self, []string{"xray"}
}
