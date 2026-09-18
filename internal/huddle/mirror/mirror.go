package mirror

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/LeafdTK/huddlecast/internal/browser"
	"github.com/LeafdTK/huddlecast/internal/browser/inject"
	"github.com/LeafdTK/huddlecast/internal/huddle"
)

//go:embed mirror.js
var script string

type Config struct {
	TeamID             string
	ChannelID          string
	ChannelName        string
	AccountName        string
	CookieD            string
	WHIPURL            string
	Token              string
	Browser            browser.Options
	DebugDir           string
	JoinTimeout        time.Duration
	RejoinBackoffMax   time.Duration
	ScreenshotInterval time.Duration
	Log                *slog.Logger
	OnStatus           func(status huddle.Status, err string)

	Engine      string
	Host        string
	Region      string
	TargetUsers []string
	PageURL     string

	VideoWidth  int
	VideoHeight int
	VideoFPS    int
}

type Engine interface {
	Run(ctx context.Context) error
	Snapshot() Snapshot
}

func NewEngine(cfg Config) Engine {
	if cfg.Engine == "chime" {
		return newChime(cfg)
	}
	return New(cfg)
}

type State struct {
	Status        string `json:"status"`
	Error         string `json:"error"`
	Publishes     int    `json:"publishes"`
	SourceVideo   string `json:"sourceVideo"`
	AudioSources  int    `json:"audioSources"`
	FramesPainted int64  `json:"framesPainted"`
	LastFrameAt   int64  `json:"lastFrameAt"`
	Tiles         int    `json:"tiles"`
	TargetUser    string `json:"targetUser"`
	Targets       string `json:"targets"`
	Seen          string `json:"seen"`
}

type Snapshot struct {
	Status       huddle.Status
	Error        string
	Participants int
	Mirror       *State
	Screenshot   []byte
	ScreenshotAt time.Time
	Account      string
	ChannelID    string
	ChannelName  string
	Since        time.Time
}

type Worker struct {
	cfg  Config
	mu   sync.Mutex
	snap Snapshot
}

func New(cfg Config) *Worker {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.Log = cfg.Log.With("mirror", cfg.ChannelName, "account", cfg.AccountName)
	return &Worker{cfg: cfg, snap: Snapshot{
		Status: huddle.StatusPending, Participants: -1, Account: cfg.AccountName,
		ChannelID: cfg.ChannelID, ChannelName: cfg.ChannelName, Since: time.Now(),
	}}
}

func (w *Worker) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap
}

func (w *Worker) setStatus(s huddle.Status, err string) {
	w.mu.Lock()
	changed := w.snap.Status != s || w.snap.Error != err
	w.snap.Status, w.snap.Error = s, err
	if changed {
		w.snap.Since = time.Now()
	}
	w.mu.Unlock()
	if changed {
		w.cfg.Log.Info("mirror status", "status", s, "err", err)
		if w.cfg.OnStatus != nil {
			w.cfg.OnStatus(s, err)
		}
	}
}

func (w *Worker) Run(ctx context.Context) error {
	backoff := 2 * time.Second
	for {
		err := w.runOnce(ctx)
		if ctx.Err() != nil {
			w.setStatus(huddle.StatusStopped, "")
			return nil
		}
		wait := backoff
		switch {
		case err == nil:
			wait = 2 * time.Second
		case errors.Is(err, browser.ErrNotLoggedIn):
			w.setStatus(huddle.StatusNeedsReauth, err.Error())
			wait = 5 * time.Minute
		case errors.Is(err, huddle.ErrAccessDenied):
			w.setStatus(huddle.StatusError, err.Error())
			wait = 5 * time.Minute
		default:
			w.setStatus(huddle.StatusError, err.Error())
			backoff = min(backoff*2, w.cfg.RejoinBackoffMax)
		}
		select {
		case <-ctx.Done():
			w.setStatus(huddle.StatusStopped, "")
			return nil
		case <-time.After(wait):
		}
	}
}

func (w *Worker) script() string {
	b, _ := json.Marshal(map[string]any{
		"whipUrl": w.cfg.WHIPURL, "token": w.cfg.Token,
		"width": w.cfg.Browser.Width, "height": w.cfg.Browser.Height, "fps": 30,
	})
	return fmt.Sprintf("window.__HUDDLECAST_MIRROR_CFG = %s;\n%s", b, script)
}

func (w *Worker) runOnce(ctx context.Context) error {
	w.setStatus(huddle.StatusLaunching, "")
	opts := w.cfg.Browser
	opts.Log = w.cfg.Log
	b, err := browser.Launch(ctx, opts)
	if err != nil {
		return err
	}
	defer b.Close()

	shim := inject.Config{Presentation: huddle.PresentationNone, Width: w.cfg.Browser.Width, Height: w.cfg.Browser.Height, FPS: 30}
	if err := b.OpenSlack(ctx, w.cfg.CookieD, shim, huddle.DeepLinkURL(w.cfg.TeamID, w.cfg.ChannelID)); err != nil {
		b.SaveDebug(w.cfg.DebugDir, "mirror-open-"+w.cfg.ChannelID)
		return err
	}
	d := &huddle.Driver{B: b, Log: w.cfg.Log}
	w.setStatus(huddle.StatusJoining, "")
	if err := d.Join(ctx, w.cfg.TeamID, w.cfg.ChannelID, w.cfg.JoinTimeout); err != nil {
		b.SaveDebug(w.cfg.DebugDir, "mirror-join-"+w.cfg.ChannelID)
		return err
	}
	b.AdoptHuddleWindow()
	w.setStatus(huddle.StatusJoined, "")
	time.Sleep(2 * time.Second)
	d.EnsureMuted()
	if err := b.InstallScript(w.script()); err != nil {
		return fmt.Errorf("install mirror script: %w", err)
	}

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var lastShot time.Time
	for {
		select {
		case <-ctx.Done():
			d.Leave()
			return nil
		case <-tick.C:
		}
		if err := b.CheckLoggedIn(); err != nil {
			return err
		}
		if b.AdoptHuddleWindow() {
			_ = b.InstallScript(w.script())
		}
		if !d.InHuddle() {
			b.SaveDebug(w.cfg.DebugDir, "mirror-dropped-"+w.cfg.ChannelID)
			return errors.New("dropped out of the source huddle")
		}
		d.EnsureMuted()
		var st State
		_ = b.Eval(`() => window.__huddlecastMirror ? window.__huddlecastMirror.snapshot() : null`, &st)
		n := d.Participants()
		w.mu.Lock()
		w.snap.Mirror = &st
		w.snap.Participants = n
		w.mu.Unlock()
		switch st.Status {
		case "live":
			w.setStatus(huddle.StatusStreaming, "")
		case "error":
			w.setStatus(huddle.StatusJoined, "publish: "+st.Error)
		}
		if time.Since(lastShot) >= w.cfg.ScreenshotInterval {
			lastShot = time.Now()
			if img, err := b.Screenshot(); err == nil {
				w.mu.Lock()
				w.snap.Screenshot, w.snap.ScreenshotAt = img, lastShot
				w.mu.Unlock()
			}
		}
	}
}
