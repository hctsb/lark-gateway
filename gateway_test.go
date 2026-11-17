package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the route command: run with GW_HELPER set, it
// copies stdin to the file GW_OUT names (appending) and exits with GW_EXIT.
// If GW_OUT.next exists, it passes that on as its next run time.
func TestMain(m *testing.M) {
	if os.Getenv("GW_HELPER") == "1" {
		in, _ := io.ReadAll(os.Stdin)
		if out := os.Getenv("GW_OUT"); out != "" {
			f, _ := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			fmt.Fprintf(f, "%s|%s|%s\n", os.Getenv("GATEWAY_REASON"), os.Getenv("GATEWAY_EVENT_ID"), strings.TrimSpace(string(in)))
			f.Close()
			if next, err := os.ReadFile(out + ".next"); err == nil && os.Getenv("GATEWAY_NEXT_RUN_FILE") != "" {
				os.WriteFile(os.Getenv("GATEWAY_NEXT_RUN_FILE"), next, 0o600)
			}
		}
		if os.Getenv("GW_EXIT") == "1" {
			fmt.Println("helper: failing on purpose")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func writeConfig(t *testing.T, toml, env string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "gateway.toml")
	if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func helperCommand() string {
	exe, _ := os.Executable()
	return strings.ReplaceAll(exe, `\`, `\\`)
}

func goodToml() string {
	return fmt.Sprintf(`
[[app]]
name = "finance"
app_id = "cli_test"
secret_env = "FINANCE_SECRET"

[[route]]
name = "bank-files"
app = "finance"
event = "im.message.receive_v1"
match = { "event.message.chat_id" = ["oc_bank"], "event.sender.sender_type" = ["user"] }
command = ["%s"]
env = ["GW_HELPER", "GW_OUT", "GW_EXIT"]
catchup = ["%s"]
`, helperCommand(), helperCommand())
}

func TestConfigOK(t *testing.T) {
	p := writeConfig(t, goodToml(), "FINANCE_SECRET=s\nGW_HELPER=1\nGW_OUT=x\nGW_EXIT=0\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Routes[0]
	if r.timeout != 10*time.Minute || r.Retries != 3 || c.Gateway.MemoryLimitMB != 32 {
		t.Fatalf("defaults not applied: %+v %+v", r, c.Gateway)
	}
	if !filepath.IsAbs(c.Gateway.SpoolDir) {
		t.Fatalf("spool dir should be absolute: %s", c.Gateway.SpoolDir)
	}
}

func TestConfigRejects(t *testing.T) {
	cases := map[string]struct{ toml, env, want string }{
		"unknown key":     {goodToml() + "\n[gateway]\nspool = \"x\"\n", "FINANCE_SECRET=s\nGW_HELPER=1\nGW_OUT=x\nGW_EXIT=0\n", "unknown settings"},
		"missing secret":  {goodToml(), "GW_HELPER=1\nGW_OUT=x\nGW_EXIT=0\n", "FINANCE_SECRET is not set"},
		"missing env key": {goodToml(), "FINANCE_SECRET=s\nGW_HELPER=1\n", "env GW_OUT is not set"},
		"unknown app":     {strings.Replace(goodToml(), `app = "finance"`, `app = "other"`, 1), "FINANCE_SECRET=s\nGW_HELPER=1\nGW_OUT=x\nGW_EXIT=0\n", `app "other" is not configured`},
		"bad command":     {strings.Replace(goodToml(), `command = ["`+helperCommand()+`"]`, `command = ["C:\\nope\\nothing.exe"]`, 1), "FINANCE_SECRET=s\nGW_HELPER=1\nGW_OUT=x\nGW_EXIT=0\n", "does not exist"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tc.toml, tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func event(id, chat, sender string) []byte {
	return []byte(fmt.Sprintf(`{"schema":"2.0","header":{"event_id":"%s","event_type":"im.message.receive_v1"},`+
		`"event":{"sender":{"sender_type":"%s"},"message":{"chat_id":"%s","message_type":"file"}}}`, id, sender, chat))
}

func TestMatching(t *testing.T) {
	r := RouteCfg{Event: "im.message.receive_v1", Match: map[string][]string{
		"event.message.chat_id": {"oc_bank"}, "event.sender.sender_type": {"user"}}}
	for _, tc := range []struct {
		body []byte
		want bool
	}{
		{event("e1", "oc_bank", "user"), true},
		{event("e2", "oc_other", "user"), false},
		{event("e3", "oc_bank", "app"), false}, // the bot's own message
	} {
		ev, err := parseEvent(tc.body)
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Matches(ev); got != tc.want {
			t.Errorf("%s: got %v want %v", ev.ID, got, tc.want)
		}
	}
	if _, err := parseEvent([]byte(`{"type":"v1 event"}`)); err == nil {
		t.Error("a v1 event without a header should be refused")
	}
}

func newTestGateway(t *testing.T, exitCode string) (*Gateway, *Route, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "ran.txt")
	p := writeConfig(t, goodToml(), "FINANCE_SECRET=s\nGW_HELPER=1\nGW_EXIT="+exitCode+"\nGW_OUT="+out+"\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	c.Routes[0].Retries = 2
	gw := &Gateway{cfg: c, health: newHealth(c), byApp: map[string][]*Route{}, alertSent: map[string]time.Time{}}
	r, err := newRoute(gw, &c.Routes[0])
	if err != nil {
		t.Fatal(err)
	}
	gw.routes = []*Route{r}
	gw.byApp["finance"] = gw.routes
	return gw, r, out
}

func TestEventsRunInOrderOnceAndCatchupAfter(t *testing.T) {
	backoff = []time.Duration{10 * time.Millisecond}
	gw, r, out := newTestGateway(t, "0")
	for _, id := range []string{"e1", "e2", "e1"} { // e1 redelivered
		if err := gw.receive("finance", event(id, "oc_bank", "user")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct arrival times
	}
	if err := gw.receive("finance", event("e3", "oc_other", "user")); err != nil { // not this route's chat
		t.Fatal(err)
	}
	r.WantCatchup()
	ctx, cancel := context.WithCancel(context.Background())
	go r.Work(ctx)
	waitFor(t, func() bool { b, _ := os.ReadFile(out); return strings.Count(string(b), "\n") >= 3 })
	cancel()

	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "event|e1|") || !strings.HasPrefix(lines[1], "event|e2|") || lines[2] != "catchup||" {
		t.Fatalf("want e1, e2, then catchup; got:\n%s", b)
	}
	if done, _ := filepath.Glob(filepath.Join(r.dir, "done", "*.json")); len(done) != 2 {
		t.Fatalf("want 2 done files, got %d", len(done))
	}
	// A restart remembers what it has seen: e2 again is still a duplicate.
	r2, _ := newRoute(gw, r.cfg)
	if !r2.seen["e2"] {
		t.Fatal("a restarted route should remember e2 from its done folder")
	}
}

func TestFailingEventIsSetAsideAfterRetries(t *testing.T) {
	backoff = []time.Duration{10 * time.Millisecond}
	gw, r, out := newTestGateway(t, "1")
	if err := gw.receive("finance", event("bad", "oc_bank", "user")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go r.Work(ctx)
	waitFor(t, func() bool { f, _ := filepath.Glob(filepath.Join(r.dir, "failed", "*.json")); return len(f) == 1 })
	cancel()
	b, _ := os.ReadFile(out)
	if n := strings.Count(string(b), "event|bad|"); n != 2 {
		t.Fatalf("want 2 attempts, got %d", n)
	}
	if h := gw.health.Routes["bank-files"]; h.LastErr == "" {
		t.Fatal("health should record the failure")
	}
}

func TestCommandSeesOnlyAllowedEnv(t *testing.T) {
	gw, r, _ := newTestGateway(t, "0")
	os.Setenv("SOMETHING_SECRET", "leak")
	defer os.Unsetenv("SOMETHING_SECRET")
	env := strings.Join(r.env("event", "e1"), "\n")
	if strings.Contains(env, "SOMETHING_SECRET") || strings.Contains(env, "FINANCE_SECRET") {
		t.Fatalf("command env leaks secrets:\n%s", env)
	}
	if !strings.Contains(env, "GATEWAY_EVENT_ID=e1") || !strings.Contains(env, "GW_HELPER=1") {
		t.Fatalf("command env missing gateway or route values:\n%s", env)
	}
	r.cfg.PassCred = true
	if env := strings.Join(r.env("event", "e1"), "\n"); !strings.Contains(env, "GATEWAY_LARK_APP_SECRET=s") {
		t.Fatal("pass_app_credentials should hand over the app credentials")
	}
	_ = gw
}

// tickingRoute is the test route with a tick every 100 ms that may be put
// off for up to an hour.
func tickingRoute(t *testing.T) (*Gateway, *Route, string) {
	gw, r, out := newTestGateway(t, "0")
	r.cfg.Tick.Command = r.cfg.Command
	r.cfg.every, r.cfg.maxWait = 100*time.Millisecond, time.Hour
	return gw, r, out
}

func ticks(out string) int {
	b, _ := os.ReadFile(out)
	return strings.Count(string(b), "tick||")
}

func TestTakeReport(t *testing.T) {
	_, r, _ := newTestGateway(t, "0")
	r.cfg.every, r.cfg.maxWait = time.Minute, 6*time.Hour
	near := func(got, want time.Time) bool { d := got.Sub(want); return d > -5*time.Second && d < 5*time.Second }
	say := func(s string) time.Time {
		p := filepath.Join(t.TempDir(), "next")
		if s != "" {
			os.WriteFile(p, []byte(s+"\r\n"), 0o600)
		}
		r.takeReport(p, "test")
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%q: the report file should be removed once read", s)
		}
		r.nextMu.Lock()
		defer r.nextMu.Unlock()
		return r.nextAt
	}
	now := time.Now()
	if at := say("none"); !near(at, now.Add(6*time.Hour)) {
		t.Errorf("none: want max_wait from now, got %v", at)
	}
	if at := say(now.Add(2 * time.Hour).Format(time.RFC3339)); !near(at, now.Add(2*time.Hour)) {
		t.Errorf("in 2 h: got %v", at)
	}
	if at := say(now.Add(-time.Hour).Format(time.RFC3339)); !near(at, now.Add(time.Minute)) {
		t.Errorf("a time already past: want tick.every from now, got %v", at)
	}
	if at := say(now.Add(48 * time.Hour).Format(time.RFC3339)); !near(at, now.Add(6*time.Hour)) {
		t.Errorf("beyond max_wait: want max_wait from now, got %v", at)
	}
	before, n := say(now.Add(3*time.Hour).Format(time.RFC3339)), r.reports.Load()
	if at := say("tomorrow-ish"); !at.Equal(before) || r.reports.Load() != n {
		t.Error("an unreadable time should be ignored")
	}
	if at := say(""); !at.Equal(before) || r.reports.Load() != n {
		t.Error("a command that writes nothing should leave the schedule alone")
	}
	if h := r.gw.health.Routes["bank-files"]; !h.NextTick.Equal(before) {
		t.Error("health should show the next tick")
	}
}

func TestTickKeepsItsPaceWhenTheCommandSaysNothing(t *testing.T) {
	_, r, out := tickingRoute(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Tick(ctx)
	waitFor(t, func() bool { return ticks(out) >= 3 })
}

func TestTickSleepsUntilTheCommandsTime(t *testing.T) {
	gw, r, out := tickingRoute(t)
	os.WriteFile(out+".next", []byte("none"), 0o600)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Tick(ctx)
	go r.Work(ctx)
	waitFor(t, func() bool { return ticks(out) == 1 })
	time.Sleep(600 * time.Millisecond)
	if n := ticks(out); n != 1 {
		t.Fatalf("after \"none\" the tick should wait max_wait; it ran %d times", n)
	}
	// An event's run can bring it forward: the send it approved is due soon.
	os.WriteFile(out+".next", []byte(time.Now().Add(300*time.Millisecond).Format(time.RFC3339Nano)), 0o600)
	if err := gw.receive("finance", event("approve", "oc_bank", "user")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return ticks(out) == 2 })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}

func TestSubscribingIsRetriedUntilLarkAgrees(t *testing.T) {
	subscribeRetry, subscribeAlertAfter = 10*time.Millisecond, 15*time.Millisecond
	gw, _, _ := newTestGateway(t, "0")
	a := gw.cfg.app("finance")
	a.SubscribeBitable = []string{"BASE1", "BASE2"}
	calls := map[string]int{}
	// BASE1 is granted at once; BASE2 only on the 4th try, as when the
	// network is still coming up after a power cut.
	sub := func(tok string) error {
		calls[tok]++
		if tok == "BASE2" && calls[tok] < 4 {
			return fmt.Errorf("dial tcp: lookup open.larksuite.com: no such host")
		}
		return nil
	}
	gw.subscribeAll(context.Background(), a, sub)
	if calls["BASE1"] != 1 || calls["BASE2"] != 4 {
		t.Fatalf("want BASE1 asked once and BASE2 until granted; got %v", calls)
	}
	if s := gw.health.Apps["finance"].Subscribed; !s["BASE1"] || !s["BASE2"] {
		t.Fatalf("health should show both subscribed: %v", s)
	}
	if _, alerted := gw.alertSent["sub:BASE2"]; !alerted {
		t.Fatal("a subscription failing longer than subscribeAlertAfter should be alerted")
	}
	if _, alerted := gw.alertSent["sub:BASE1"]; alerted {
		t.Fatal("a granted subscription should not be alerted")
	}
}

func TestSubscribingStopsWithTheGateway(t *testing.T) {
	subscribeRetry, subscribeAlertAfter = 10*time.Millisecond, time.Hour
	gw, _, _ := newTestGateway(t, "0")
	a := gw.cfg.app("finance")
	a.SubscribeBitable = []string{"BASE1"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		gw.subscribeAll(ctx, a, func(string) error { return fmt.Errorf("refused") })
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscribeAll should stop when the gateway stops")
	}
	if gw.health.Apps["finance"].Subscribed["BASE1"] {
		t.Fatal("health should show BASE1 not subscribed")
	}
}
