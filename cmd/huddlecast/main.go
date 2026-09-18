package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LeafdTK/huddlecast/internal/browser"
	"github.com/LeafdTK/huddlecast/internal/browser/inject"
	"github.com/LeafdTK/huddlecast/internal/config"
	"github.com/LeafdTK/huddlecast/internal/huddle"
	"github.com/LeafdTK/huddlecast/internal/media"
	"github.com/LeafdTK/huddlecast/internal/recording"
	"github.com/LeafdTK/huddlecast/internal/session"
	"github.com/LeafdTK/huddlecast/internal/slackapp"
	"github.com/LeafdTK/huddlecast/internal/store"
	"github.com/LeafdTK/huddlecast/internal/web"
)

const usage = `huddlecast — stream one source into many slack huddles

usage:
  huddlecast serve --config huddlecast.yml
  huddlecast probe --config huddlecast.yml --channel C0123 [--account NAME] [--join] [--share]
  huddlecast check --config huddlecast.yml
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	loadDotEnv(".env")
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:], log)
	case "probe":
		err = probe(os.Args[2:], log)
	case "check":
		err = check(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func loadDotEnv(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
}

func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	path := fs.String("config", "huddlecast.yml", "config file")
	_ = fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("config ok: %d account(s), team %s, web auth %s\n", len(cfg.Accounts), cfg.Slack.TeamID, cfg.Web.Auth)
	return nil
}

func serve(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "huddlecast.yml", "config file")
	_ = fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(filepath.Join(cfg.DataDir, "huddlecast.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	m := media.New(cfg.MediaMTX.APIURL, cfg.MediaMTX.InternalURL, cfg.MediaMTX.PublicHost)
	if err := m.Healthy(ctx); err != nil {
		log.Warn("mediamtx not reachable yet", "err", err)
	}
	mgr := session.New(ctx, cfg, st, m, log)
	app := slackapp.New(cfg, st, mgr, log)
	if err := app.SeedWhitelist(ctx); err != nil {
		return err
	}
	up, err := recording.New(cfg.Recording.R2, cfg.MediaMTX.RecordingsDir, st, log)
	if err != nil {
		return err
	}
	go up.Run(ctx)
	srv, err := web.New(cfg, st, mgr, app, m, up, log)
	if err != nil {
		return err
	}
	if err := mgr.Resume(ctx); err != nil {
		return err
	}

	if cfg.Slack.SocketMode() {
		go func() {
			backoff := 5 * time.Second
			for ctx.Err() == nil {
				err := app.Run(ctx)
				if ctx.Err() != nil {
					return
				}
				log.Error("slack connection failed, retrying", "err", err, "in", backoff)
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 2*time.Minute)
			}
		}()
	} else {
		log.Info("slack app disabled (no bot/app token): slash commands and the chat feed are off; web ui, cookie sign-in and streaming still work")
	}
	err = srv.ListenAndServe(ctx)
	stop()
	mgr.StopAll()
	return err
}

func probe(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	path := fs.String("config", "huddlecast.yml", "config file")
	channel := fs.String("channel", "", "channel id to open")
	account := fs.String("account", "", "account name (default: first)")
	join := fs.Bool("join", false, "click join and dump the in-huddle UI too")
	share := fs.Bool("share", false, "after joining, try to start a screen share")
	whep := fs.String("whep", "", "WHEP url to feed (default: mediamtx live/probe)")
	stay := fs.Duration("stay", 0, "how long to stay in the huddle after probing")
	trace := fs.Bool("trace", false, "log slack/chime api traffic to <data>/probe/trace.jsonl")
	noShim := fs.Bool("no-shim", false, "do not install the media shim (a/b debugging)")
	_ = fs.Parse(args)
	if *channel == "" {
		return errors.New("--channel is required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	acct := cfg.Accounts[0]
	if *account != "" {
		var ok bool
		if acct, ok = cfg.Account(*account); !ok {
			return fmt.Errorf("no account %q", *account)
		}
	}
	m := media.New(cfg.MediaMTX.APIURL, cfg.MediaMTX.InternalURL, cfg.MediaMTX.PublicHost)
	if *whep == "" {
		*whep = m.WHEPURL(media.PushPath("probe"))
	}
	outDir := filepath.Join(cfg.DataDir, "probe")
	_ = os.MkdirAll(outDir, 0o755)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	b, err := browser.Launch(ctx, browser.Options{
		Bin: cfg.Browser.Bin, Headless: cfg.Browser.Headless, ProfileDir: filepath.Join(cfg.DataDir, "profiles", "probe"),
		Width: cfg.Browser.Width, Height: cfg.Browser.Height, Log: log,
	})
	if err != nil {
		return err
	}
	defer b.Close()
	shim := inject.Config{WHEPURL: *whep, Presentation: huddle.PresentationScreen, Width: cfg.Browser.Width, Height: cfg.Browser.Height, FPS: cfg.Browser.FPS, Debug: true}
	if *trace {
		stopTrace := b.TraceOnOpen(filepath.Join(outDir, "trace.jsonl"), func(u string) bool {
			return strings.Contains(u, "slack.com/api/") || strings.Contains(u, "chime") || strings.Contains(u, "amazonaws") || strings.Contains(u, "free-willy") || strings.Contains(u, "free_willy")
		})
		defer stopTrace()
	}
	if *noShim {
		shim.Presentation = "off"
	}
	if err := b.OpenSlack(ctx, acct.CookieD, shim, huddle.DeepLinkURL(cfg.Slack.TeamID, *channel)); err != nil {
		b.SaveDebug(outDir, "open")
		return err
	}
	time.Sleep(5 * time.Second)
	d := &huddle.Driver{B: b, Log: log}
	dump := func(label string) {
		b.SaveDebug(outDir, label)
		btns, err := d.Probe()
		if err != nil {
			log.Error("probe", "err", err)
			return
		}
		f := filepath.Join(outDir, label+"-buttons.json")
		data, _ := json.MarshalIndent(btns, "", "  ")
		_ = os.WriteFile(f, data, 0o644)
		fmt.Printf("\n== %s (%s) — %d interactive elements → %s\n", label, b.URL(), len(btns), f)
		for _, x := range btns {
			if x.Visible && (x.DataQA != "" || x.AriaLabel != "" || x.Text != "") {
				fmt.Printf("  %-6s data-qa=%-45q aria=%-45q text=%q\n", x.Tag, x.DataQA, x.AriaLabel, x.Text)
			}
		}
	}
	dump("prejoin")
	fmt.Println("\nselector check:")
	for name, sel := range map[string]huddle.Selector{"Join": huddle.Join, "Toggle": huddle.Toggle, "Leave": huddle.Leave, "AccessDenied": huddle.AccessDenied} {
		fmt.Printf("  %-13s %v\n", name, d.Find(sel) != nil)
	}
	if !*join {
		return nil
	}
	if err := d.Join(ctx, cfg.Slack.TeamID, *channel, cfg.Huddle.JoinTimeout); err != nil {
		dump("join-failed")
		return err
	}
	b.AdoptHuddleWindow()
	time.Sleep(6 * time.Second)
	b.AdoptHuddleWindow()
	fmt.Println("pages:", b.PageURLs())
	var uiLen int
	_ = b.EvalUI(`() => document.documentElement.outerHTML.length`, &uiLen)
	fmt.Println("huddle window html length:", uiLen)
	dump("in-huddle")
	fmt.Println("\nselector check:")
	for name, sel := range map[string]huddle.Selector{"Leave": huddle.Leave, "Unmute": huddle.Unmute, "Mute": huddle.Mute, "ShareScreen": huddle.ShareScreen, "StopShare": huddle.StopShare, "CameraOn": huddle.CameraOn, "CameraOff": huddle.CameraOff, "MemberCount": huddle.MemberCount} {
		fmt.Printf("  %-13s %v\n", name, d.Find(sel) != nil)
	}
	fmt.Printf("  participants  %d\n", d.Participants())
	if *share {
		d.EnsureUnmuted()
		if err := d.StartShare(ctx); err != nil {
			dump("share-failed")
			fmt.Println("share failed:", err)
		} else {
			time.Sleep(3 * time.Second)
			dump("sharing")
		}
	}
	if s, err := b.ShimState(); err == nil {
		j, _ := json.MarshalIndent(s, "", "  ")
		fmt.Printf("\nshim state:\n%s\n", j)
	}
	if *stay > 0 {
		fmt.Printf("\nstaying for %s (ctrl-c to leave early)\n", *stay)
		select {
		case <-ctx.Done():
		case <-time.After(*stay):
		}
	}
	d.Leave()
	return nil
}
