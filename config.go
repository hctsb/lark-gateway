package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is gateway.toml. Secrets never appear in it: an app names the .env
// key that holds its secret, and a route names the .env keys its command may
// see.
type Config struct {
	Gateway GatewayCfg `toml:"gateway"`
	Alert   AlertCfg   `toml:"alert"`
	Apps    []AppCfg   `toml:"app"`
	Routes  []RouteCfg `toml:"route"`

	dir     string            // folder gateway.toml is in; relative paths start here
	secrets map[string]string // .env, then the process environment on top
}

type GatewayCfg struct {
	SpoolDir      string `toml:"spool_dir"`       // default "spool"
	LogDir        string `toml:"log_dir"`         // default "logs"
	HealthAddr    string `toml:"health_addr"`     // default "127.0.0.1:8791"; also stops a second copy starting
	MemoryLimitMB int    `toml:"memory_limit_mb"` // default 32
	KeepDoneDays  int    `toml:"keep_done_days"`  // default 14
}

type AlertCfg struct {
	App         string `toml:"app"`          // which app posts the alert
	ChatID      string `toml:"chat_id"`      // the Lark chat that receives it
	DownMinutes int    `toml:"down_minutes"` // default 5
}

type AppCfg struct {
	Name             string   `toml:"name"`
	AppID            string   `toml:"app_id"`
	SecretEnv        string   `toml:"secret_env"`
	Domain           string   `toml:"domain"`            // "lark" (default) or "feishu"
	SubscribeBitable []string `toml:"subscribe_bitable"` // Base tokens whose record changes this app should receive
	IgnoreEvents     []string `toml:"ignore_events"`     // more event types to accept without routing
}

type RouteCfg struct {
	Name     string              `toml:"name"`
	App      string              `toml:"app"`
	Event    string              `toml:"event"`
	Match    map[string][]string `toml:"match"`
	Command  []string            `toml:"command"`
	Workdir  string              `toml:"workdir"`
	Env      []string            `toml:"env"`
	Timeout  string              `toml:"timeout"` // default "10m"
	Retries  int                 `toml:"retries"` // default 3
	Catchup  []string            `toml:"catchup"`
	Tick     TickCfg             `toml:"tick"`
	PassCred bool                `toml:"pass_app_credentials"`

	timeout time.Duration
	every   time.Duration
	maxWait time.Duration
}

type TickCfg struct {
	Every   string   `toml:"every"`    // how often, unless the command says when it is next needed
	MaxWait string   `toml:"max_wait"` // the longest a command may put it off; default "6h"
	Command []string `toml:"command"`
}

// LoadConfig reads and checks gateway.toml and the .env beside it. Anything
// unknown or missing stops the gateway with a message saying what and where.
func LoadConfig(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if extra := md.Undecoded(); len(extra) > 0 {
		keys := make([]string, len(extra))
		for i, k := range extra {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%s: unknown settings (typo?): %s", path, strings.Join(keys, ", "))
	}
	abs, _ := filepath.Abs(path)
	c.dir = filepath.Dir(abs)
	c.secrets = readDotenv(filepath.Join(c.dir, ".env"))
	c.defaults()
	if err := c.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) defaults() {
	g := &c.Gateway
	if g.SpoolDir == "" {
		g.SpoolDir = "spool"
	}
	if g.LogDir == "" {
		g.LogDir = "logs"
	}
	if g.HealthAddr == "" {
		g.HealthAddr = "127.0.0.1:8791"
	}
	if g.MemoryLimitMB == 0 {
		g.MemoryLimitMB = 32
	}
	if g.KeepDoneDays == 0 {
		g.KeepDoneDays = 14
	}
	g.SpoolDir, g.LogDir = c.abs(g.SpoolDir), c.abs(g.LogDir)
	if c.Alert.DownMinutes == 0 {
		c.Alert.DownMinutes = 5
	}
	for i := range c.Apps {
		if c.Apps[i].Domain == "" {
			c.Apps[i].Domain = "lark"
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Timeout == "" {
			r.Timeout = "10m"
		}
		if r.Retries == 0 {
			r.Retries = 3
		}
		if r.Workdir != "" {
			r.Workdir = c.abs(r.Workdir)
		}
		if r.Tick.Every != "" && r.Tick.MaxWait == "" {
			r.Tick.MaxWait = "6h"
		}
	}
}

func (c *Config) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.dir, p)
}

func (c *Config) check() error {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	apps := map[string]bool{}
	for _, a := range c.Apps {
		switch {
		case a.Name == "":
			bad("an [[app]] has no name")
		case apps[a.Name]:
			bad("app %q appears twice", a.Name)
		}
		apps[a.Name] = true
		if a.AppID == "" {
			bad("app %q: app_id is missing", a.Name)
		}
		if a.SecretEnv == "" {
			bad("app %q: secret_env is missing", a.Name)
		} else if c.secret(a.SecretEnv) == "" {
			bad("app %q: %s is not set in .env", a.Name, a.SecretEnv)
		}
		if a.Domain != "lark" && a.Domain != "feishu" {
			bad("app %q: domain must be \"lark\" or \"feishu\"", a.Name)
		}
	}
	if len(c.Apps) == 0 {
		bad("no [[app]] configured")
	}

	routes := map[string]bool{}
	for i := range c.Routes {
		r := &c.Routes[i]
		name := r.Name
		switch {
		case name == "":
			bad("a [[route]] has no name")
			name = fmt.Sprintf("#%d", i+1)
		case routes[name]:
			bad("route %q appears twice", name)
		case strings.ContainsAny(name, `\/:*?"<>| `):
			bad("route %q: use letters, digits and - only (it names a folder)", name)
		}
		routes[name] = true
		if !apps[r.App] {
			bad("route %q: app %q is not configured", name, r.App)
		}
		if r.Event == "" && r.Tick.Every == "" {
			bad("route %q: needs an event, a tick, or both", name)
		}
		if r.Event != "" {
			if err := checkCommand(r.Command, r.Workdir); err != nil {
				bad("route %q: command: %v", name, err)
			}
		}
		if len(r.Catchup) > 0 {
			if err := checkCommand(r.Catchup, r.Workdir); err != nil {
				bad("route %q: catchup: %v", name, err)
			}
		}
		if r.Tick.Every != "" {
			d, err := time.ParseDuration(r.Tick.Every)
			if err != nil || d < 10*time.Second {
				bad("route %q: tick.every must be a duration of at least 10s, like \"1m\"", name)
			}
			r.every = d
			if mw, err := time.ParseDuration(r.Tick.MaxWait); err != nil || mw < d {
				bad("route %q: tick.max_wait must be a duration no shorter than tick.every, like \"6h\"", name)
			} else {
				r.maxWait = mw
			}
			if err := checkCommand(r.Tick.Command, r.Workdir); err != nil {
				bad("route %q: tick.command: %v", name, err)
			}
		}
		d, err := time.ParseDuration(r.Timeout)
		if err != nil || d <= 0 {
			bad("route %q: timeout must be a duration like \"10m\"", name)
		}
		r.timeout = d
		if r.Workdir != "" {
			if st, err := os.Stat(r.Workdir); err != nil || !st.IsDir() {
				bad("route %q: workdir %s does not exist", name, r.Workdir)
			}
		}
		for _, k := range r.Env {
			if c.secret(k) == "" {
				bad("route %q: env %s is not set in .env", name, k)
			}
		}
		for k, v := range r.Match {
			if k == "" || len(v) == 0 {
				bad("route %q: match %q needs at least one value", name, k)
			}
		}
	}
	if len(c.Routes) == 0 {
		bad("no [[route]] configured")
	}

	if c.Alert.ChatID != "" && !apps[c.Alert.App] {
		bad("alert: app %q is not configured", c.Alert.App)
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func checkCommand(cmd []string, workdir string) error {
	if len(cmd) == 0 || cmd[0] == "" {
		return fmt.Errorf("is empty")
	}
	exe := cmd[0]
	if !filepath.IsAbs(exe) && workdir != "" && strings.ContainsAny(exe, `\/`) {
		exe = filepath.Join(workdir, exe)
	}
	if filepath.IsAbs(exe) {
		if _, err := os.Stat(exe); err != nil {
			return fmt.Errorf("%s does not exist", exe)
		}
		return nil
	}
	if _, err := exec.LookPath(exe); err != nil {
		return fmt.Errorf("%s is not on PATH", exe)
	}
	return nil
}

// secret is the process environment first, then .env.
func (c *Config) secret(key string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return c.secrets[key]
}

func (c *Config) app(name string) *AppCfg {
	for i := range c.Apps {
		if c.Apps[i].Name == name {
			return &c.Apps[i]
		}
	}
	return nil
}

// readDotenv reads KEY=VALUE lines; blank lines and # comments are skipped.
func readDotenv(path string) map[string]string {
	vals := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return vals
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return vals
}
