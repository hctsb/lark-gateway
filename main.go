// Command lark-gateway holds Lark's WebSocket ("persistent connection") for
// each configured Lark app and runs a fixed command for each event a route in
// gateway.toml asks for. The machine needs no public address and nothing polls
// Lark.
//
// It decides nothing about the business. A route says: for this app, this
// event type, matching these fields, run this command with the event JSON on
// stdin. What the event means is the command's job, under its own tests.
//
// Guarantees, in order of importance:
//   - an event is on disk (spool\<route>\pending) before Lark is told it
//     arrived, so a crash, a restart or a failing command never loses it;
//   - a route runs one event at a time, oldest first, retrying a failure,
//     then setting it aside in spool\<route>\failed and alerting;
//   - after every (re)connect each route's catch-up command runs, for anything
//     that happened while the connection was down (Lark does not replay);
//   - a redelivered event is ignored; only one gateway runs per machine;
//   - a route's tick runs when its command last said it is next needed
//     (GATEWAY_NEXT_RUN_FILE), or every tick.every if it says nothing.
//
// Only one connection may exist per Lark app anywhere: Lark hands each event
// to one of an app's connections at random. Never run a second copy elsewhere
// with the same app.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
	"gopkg.in/natefinch/lumberjack.v2"
)

type Gateway struct {
	cfg    *Config
	health *Health
	routes []*Route
	byApp  map[string][]*Route
	host   string

	alertMu   sync.Mutex
	alertSent map[string]time.Time
}

func main() {
	exe, _ := os.Executable()
	def := filepath.Join(filepath.Dir(exe), "gateway.toml")
	if _, err := os.Stat(def); err != nil {
		def = "gateway.toml" // `go run .`
	}
	cfgPath := flag.String("config", def, "path to gateway.toml")
	checkOnly := flag.Bool("check", false, "check gateway.toml and .env, print the routes, and exit")
	testAlert := flag.Bool("test-alert", false, "post a test message to the alert chat, and exit")
	flag.Parse()

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "LarkGateway will not start:", err)
		os.Exit(2)
	}
	if *checkOnly {
		for _, r := range cfg.Routes {
			fmt.Printf("route %-20s app %-12s event %-40s match %v\n", r.Name, r.App, r.Event, r.Match)
		}
		fmt.Println("gateway.toml and .env are OK")
		return
	}
	if *testAlert {
		gw := &Gateway{cfg: cfg}
		gw.host, _ = os.Hostname()
		if err := gw.postAlert("test alert. If you can read this, alerts work."); err != nil {
			fmt.Fprintln(os.Stderr, "test alert failed:", err)
			os.Exit(1)
		}
		fmt.Println("test alert posted")
		return
	}
	debug.SetMemoryLimit(int64(cfg.Gateway.MemoryLimitMB) << 20)
	setupLogging(cfg.Gateway.LogDir)

	// The health address doubles as a lock: a second gateway on this machine
	// cannot bind it and stops here, before touching any Lark connection.
	ln, err := net.Listen("tcp", cfg.Gateway.HealthAddr)
	if err != nil {
		slog.Error("another LarkGateway is already running on this machine (health address in use)", "addr", cfg.Gateway.HealthAddr, "err", err)
		os.Exit(3)
	}

	gw := &Gateway{cfg: cfg, health: newHealth(cfg), byApp: map[string][]*Route{}, alertSent: map[string]time.Time{}}
	gw.host, _ = os.Hostname()
	for i := range cfg.Routes {
		r, err := newRoute(gw, &cfg.Routes[i])
		if err != nil {
			slog.Error("cannot prepare route spool", "route", cfg.Routes[i].Name, "err", err)
			os.Exit(2)
		}
		gw.routes = append(gw.routes, r)
		gw.byApp[r.cfg.App] = append(gw.byApp[r.cfg.App], r)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go http.Serve(ln, gw.health)
	for _, r := range gw.routes {
		go r.Work(ctx)
		if r.cfg.every > 0 {
			go r.Tick(ctx)
		}
	}
	go gw.watch(ctx)
	slog.Info("LarkGateway started", "config", *cfgPath, "apps", len(cfg.Apps), "routes", len(cfg.Routes), "health", "http://"+cfg.Gateway.HealthAddr+"/")

	var wg sync.WaitGroup
	for i := range cfg.Apps {
		wg.Add(1)
		go func(a *AppCfg) { defer wg.Done(); gw.runApp(ctx, a) }(&cfg.Apps[i])
	}
	wg.Wait()
	slog.Info("LarkGateway stopped")
}

// housekeepingEvents are accepted and ignored unless a route asks for them.
var housekeepingEvents = []string{
	"im.chat.member.bot.added_v1", "im.chat.member.bot.deleted_v1",
	"im.chat.member.user.added_v1", "im.chat.member.user.deleted_v1", "im.chat.member.user.withdrawn_v1",
	"im.chat.updated_v1", "im.chat.disbanded_v1",
	"im.message.message_read_v1", "im.message.recalled_v1",
	"im.message.reaction.created_v1", "im.message.reaction.deleted_v1",
	"im.chat.access_event.bot_p2p_chat_entered_v1", "application.bot.menu_v6",
}

func domainURL(d string) string {
	if d == "feishu" {
		return lark.FeishuBaseUrl
	}
	return lark.LarkBaseUrl
}

// runApp keeps one app's WebSocket open until ctx ends. The SDK reconnects by
// itself; if it gives up entirely, this starts it again a minute later.
func (g *Gateway) runApp(ctx context.Context, a *AppCfg) {
	secret := g.cfg.secret(a.SecretEnv)
	domain := domainURL(a.Domain)
	resubscribe := make(chan struct{}, 1)
	go g.keepSubscribed(ctx, a, secret, domain, resubscribe)

	d := dispatcher.NewEventDispatcher("", "")
	types := map[string]bool{}
	for _, r := range g.byApp[a.Name] {
		if r.cfg.Event != "" {
			types[r.cfg.Event] = true
		}
	}
	for t := range types {
		d.OnCustomizedEvent(t, func(_ context.Context, req *larkevent.EventReq) error {
			return g.receive(a.Name, req.Body)
		})
	}
	// Lark keeps resending an event the app refuses, and the SDK refuses any
	// type without a handler. Accept the housekeeping events Lark sends
	// whether asked or not (bot added to a group, message read, ...) and any
	// the app lists in ignore_events, logging them without routing.
	for _, t := range append(append([]string{}, housekeepingEvents...), a.IgnoreEvents...) {
		if types[t] {
			continue
		}
		types[t] = true
		d.OnCustomizedEvent(t, func(_ context.Context, req *larkevent.EventReq) error {
			if ev, err := parseEvent(req.Body); err == nil {
				slog.Info("event accepted, no route", "app", a.Name, "type", ev.Type, "event_id", ev.ID)
			}
			return nil
		})
	}
	up := func() {
		slog.Info("connected", "app", a.Name)
		g.health.connected(a.Name, true)
		select {
		case resubscribe <- struct{}{}:
		default:
		}
		for _, r := range g.byApp[a.Name] {
			r.WantCatchup()
		}
	}
	down := func() {
		slog.Warn("connection lost", "app", a.Name)
		g.health.connected(a.Name, false)
	}
	cli := larkws.NewClient(a.AppID, secret,
		larkws.WithDomain(domain),
		larkws.WithEventHandler(d),
		larkws.WithOnReady(up),
		larkws.WithOnReconnected(up),
		larkws.WithOnReconnecting(down),
		larkws.WithOnDisconnected(down),
		larkws.WithOnError(func(err error) { g.health.appError(a.Name, err.Error()) }),
		larkws.WithLogger(sdkLogger{app: a.Name}),
		larkws.WithLogLevel(larkcore.LogLevelInfo))
	for ctx.Err() == nil {
		slog.Info("connecting", "app", a.Name, "app_id", a.AppID)
		err := cli.Start(ctx)
		if ctx.Err() != nil {
			return
		}
		g.health.connected(a.Name, false)
		if err != nil {
			g.health.appError(a.Name, err.Error())
		}
		slog.Error("connection ended; retrying in 1 minute", "app", a.Name, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(time.Minute):
		}
	}
}

// How often a failed subscription is tried again, and how long it may keep
// failing before an alert: just after a power cut the network can take a few
// minutes to come up (a listener without this retry once died of exactly that
// and stayed down 19 hours).
var (
	subscribeRetry      = time.Minute
	subscribeAlertAfter = 5 * time.Minute
)

// keepSubscribed asks Lark to send record changes of the listed Bases to the
// app, each time the app (re)connects, and keeps asking every subscribeRetry
// until Lark has agreed to every one. The app's bot must be in each Base with
// "Can manage". A failure lasting subscribeAlertAfter is alerted; it never
// stops the app's other routes.
func (g *Gateway) keepSubscribed(ctx context.Context, a *AppCfg, secret, domain string, connected <-chan struct{}) {
	if len(a.SubscribeBitable) == 0 {
		return
	}
	api := lark.NewClient(a.AppID, secret, lark.WithOpenBaseUrl(domain))
	sub := func(tok string) error {
		resp, err := api.Drive.V1.File.Subscribe(ctx, larkdrive.NewSubscribeFileReqBuilder().FileToken(tok).FileType("bitable").Build())
		switch {
		case err != nil:
			return err
		case !resp.Success():
			return fmt.Errorf("refused: code %d %s (is the bot in the Base with Can manage?)", resp.Code, resp.Msg)
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-connected:
		}
		g.subscribeAll(ctx, a, subscribeBase(sub))
	}
}

// subscribeBase asks Lark for one Base's record changes.
type subscribeBase func(token string) error

// subscribeAll asks for every Base until each has been granted, retrying the
// rest every subscribeRetry.
func (g *Gateway) subscribeAll(ctx context.Context, a *AppCfg, sub subscribeBase) {
	pending, since := a.SubscribeBitable, time.Now()
	for len(pending) > 0 {
		var left []string
		for _, tok := range pending {
			err := sub(tok)
			if err == nil {
				slog.Info("subscribed to Base", "app", a.Name, "base", tok)
				g.health.subscribed(a.Name, tok, true)
				continue
			}
			left = append(left, tok)
			g.health.subscribed(a.Name, tok, false)
			slog.Warn("subscribing to Base failed; will retry", "app", a.Name, "base", tok, "retry_in", subscribeRetry.String(), "err", err)
			if time.Since(since) >= subscribeAlertAfter {
				g.alert(fmt.Sprintf("App %s: still not subscribed to Base %s after %s: %v", a.Name, tok,
					time.Since(since).Round(time.Minute), err), "sub:"+tok)
			}
		}
		if pending = left; len(pending) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(subscribeRetry):
		}
	}
}

// receive is called by the SDK for each event. It must answer within about
// three seconds or Lark sends the event again, so it only spools. An error
// (the disk refused the write) makes Lark send the event again later.
func (g *Gateway) receive(app string, body []byte) error {
	ev, err := parseEvent(body)
	if err != nil {
		slog.Warn("event not routed", "app", app, "err", err)
		return nil
	}
	g.health.event(app)
	matched := 0
	var errs []error
	for _, r := range g.byApp[app] {
		if !r.cfg.Matches(ev) {
			continue
		}
		matched++
		if err := r.Spool(ev); err != nil {
			slog.Error("could not spool event", "route", r.cfg.Name, "event_id", ev.ID, "err", err)
			errs = append(errs, err)
		}
	}
	slog.Info("event", "app", app, "type", ev.Type, "event_id", ev.ID, "routes", matched)
	return errors.Join(errs...)
}

// watch alerts on connections that stay down, keeps health.json current and
// prunes old done files.
func (g *Gateway) watch(ctx context.Context) {
	stateDir := filepath.Join(g.cfg.dir, "state")
	limit := time.Duration(g.cfg.Alert.DownMinutes) * time.Minute
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	lastPrune := time.Time{}
	for {
		for _, app := range g.health.downTooLong(limit) {
			g.alert(fmt.Sprintf("App %s has been disconnected from Lark for over %d minutes. Events will be caught up when it reconnects.", app, g.cfg.Alert.DownMinutes), "down:"+app)
		}
		g.health.writeFile(stateDir)
		if time.Since(lastPrune) > 24*time.Hour {
			for _, r := range g.routes {
				r.PruneDone(time.Duration(g.cfg.Gateway.KeepDoneDays) * 24 * time.Hour)
			}
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// alert logs the message and, if [alert] is configured, posts it to the alert
// chat. The same key is posted at most once every 30 minutes.
func (g *Gateway) alert(msg, key string) {
	slog.Error("ALERT " + msg)
	g.alertMu.Lock()
	if t, ok := g.alertSent[key]; ok && time.Since(t) < 30*time.Minute {
		g.alertMu.Unlock()
		return
	}
	g.alertSent[key] = time.Now()
	g.alertMu.Unlock()

	go func() {
		if err := g.postAlert(msg); err != nil {
			slog.Error("could not post alert to Lark", "err", err)
		}
	}()
}

// postAlert sends one message to the alert chat over Lark's HTTPS API, not
// the WebSocket, so it still works when a WebSocket is what broke.
func (g *Gateway) postAlert(msg string) error {
	a := g.cfg.app(g.cfg.Alert.App)
	if g.cfg.Alert.ChatID == "" || a == nil {
		return fmt.Errorf("no [alert] chat configured")
	}
	api := lark.NewClient(a.AppID, g.cfg.secret(a.SecretEnv), lark.WithOpenBaseUrl(domainURL(a.Domain)))
	content, _ := json.Marshal(map[string]string{"text": fmt.Sprintf("LarkGateway on %s: %s", g.host, msg)})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := api.Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().
		ReceiveIdType("chat_id").
		Body(larkim.NewCreateMessageReqBodyBuilder().ReceiveId(g.cfg.Alert.ChatID).MsgType("text").Content(string(content)).Build()).
		Build())
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("Lark refused it: code %d %s", resp.Code, resp.Msg)
	}
	return nil
}

func setupLogging(dir string) {
	_ = os.MkdirAll(dir, 0o700)
	file := &lumberjack.Logger{Filename: filepath.Join(dir, "gateway.log"), MaxSize: 10, MaxBackups: 10, MaxAge: 90}
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.MultiWriter(file, os.Stdout), nil)))
}

// sdkLogger sends the Lark SDK's own messages to the same log.
type sdkLogger struct{ app string }

func (l sdkLogger) Debug(_ context.Context, a ...any) {}
func (l sdkLogger) Info(_ context.Context, a ...any) {
	slog.Info(fmt.Sprint(a...), "app", l.app, "from", "lark-sdk")
}
func (l sdkLogger) Warn(_ context.Context, a ...any) {
	slog.Warn(fmt.Sprint(a...), "app", l.app, "from", "lark-sdk")
}
func (l sdkLogger) Error(_ context.Context, a ...any) {
	slog.Error(fmt.Sprint(a...), "app", l.app, "from", "lark-sdk")
}
