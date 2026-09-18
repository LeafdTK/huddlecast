package huddle

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/LeafdTK/huddlecast/internal/browser"
	"github.com/LeafdTK/huddlecast/internal/browser/inject"
)

type Status string

const (
	StatusPending     Status = "pending"
	StatusLaunching   Status = "launching"
	StatusJoining     Status = "joining"
	StatusJoined      Status = "joined"
	StatusStreaming   Status = "streaming"
	StatusWaiting     Status = "waiting"
	StatusNeedsReauth Status = "needs_reauth"
	StatusError       Status = "error"
	StatusStopped     Status = "stopped"
)

const (
	PresentationScreen = "screen"
	PresentationCamera = "camera"
	PresentationBoth   = "both"
	PresentationNone   = "none"
)

func ValidPresentation(p string) bool {
	switch p {
	case PresentationScreen, PresentationCamera, PresentationBoth:
		return true
	}
	return false
}

type Config struct {
	TeamID              string
	ChannelID           string
	ChannelName         string
	AccountName         string
	CookieD             string
	WHEPURL             string
	Presentation        string
	Browser             browser.Options
	DebugDir            string
	JoinTimeout         time.Duration
	LeaveWhenAloneAfter time.Duration
	RejoinBackoffMax    time.Duration
	ScreenshotInterval  time.Duration
	Log                 *slog.Logger
	OnStatus            func(status Status, err string)

	Engine  string
	Host    string
	Region  string
	PageURL string

	VideoWidth   int
	VideoHeight  int
	VideoFPS     int
	VideoMaxKbps int
	AudioProfile string
	AudioMaxKbps int
}

type TargetEngine interface {
	Run(ctx context.Context) error
	SetPresentation(p string)
	Snapshot() Snapshot
}

func NewTargetEngine(cfg Config) TargetEngine {
	if cfg.Engine == "chime" {
		return newChimeTarget(cfg)
	}
	return New(cfg)
}

type Stats struct {
	SourceRes     string
	SourceFPS     int
	SourceKbps    int
	PaintFPS      int
	TargetFPS     int
	FramesDropped int
	DropRate      int
	Standby       bool
	MicLevel      int
}

type Snapshot struct {
	Status       Status
	Error        string
	Participants int
	Shim         *browser.ShimState
	Stats        *Stats
	Screenshot   []byte
	ScreenshotAt time.Time
	URL          string
	Account      string
	ChannelID    string
	ChannelName  string
	Presentation string
	Since        time.Time
}

type Worker struct {
	cfg    Config
	mu     sync.Mutex
	snap   Snapshot
	presCh chan string
}

func New(cfg Config) *Worker {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.Log = cfg.Log.With("channel", cfg.ChannelName, "account", cfg.AccountName)
	return &Worker{
		cfg: cfg,
		snap: Snapshot{
			Status: StatusPending, Participants: -1, Account: cfg.AccountName,
			ChannelID: cfg.ChannelID, ChannelName: cfg.ChannelName, Presentation: cfg.Presentation, Since: time.Now(),
		},
		presCh: make(chan string, 4),
	}
}

func (w *Worker) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap
}

func (w *Worker) SetPresentation(p string) {
	w.mu.Lock()
	w.snap.Presentation = p
	w.mu.Unlock()
	select {
	case w.presCh <- p:
	default:
	}
}

func (w *Worker) setStatus(s Status, err string) {
	w.mu.Lock()
	changed := w.snap.Status != s || w.snap.Error != err
	w.snap.Status = s
	w.snap.Error = err
	if changed {
		w.snap.Since = time.Now()
	}
	w.mu.Unlock()
	if changed {
		w.cfg.Log.Info("target status", "status", s, "err", err)
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
			w.setStatus(StatusStopped, "")
			return nil
		}
		wait := backoff
		switch {
		case err == nil:
			wait = 2 * time.Second
		case errors.Is(err, browser.ErrNotLoggedIn):
			w.setStatus(StatusNeedsReauth, err.Error())
			wait = 5 * time.Minute
		case errors.Is(err, ErrAccessDenied):
			w.setStatus(StatusError, err.Error())
			wait = 5 * time.Minute
		default:
			w.setStatus(StatusError, err.Error())
			backoff = min(backoff*2, w.cfg.RejoinBackoffMax)
		}
		select {
		case <-ctx.Done():
			w.setStatus(StatusStopped, "")
			return nil
		case <-time.After(wait):
		}
	}
}

func (w *Worker) runOnce(ctx context.Context) error {
	w.setStatus(StatusLaunching, "")
	opts := w.cfg.Browser
	opts.Log = w.cfg.Log
	b, err := browser.Launch(ctx, opts)
	if err != nil {
		return err
	}
	defer b.Close()

	shim := inject.Config{
		WHEPURL: w.cfg.WHEPURL, Presentation: w.cfg.Presentation,
		Width: w.cfg.Browser.Width, Height: w.cfg.Browser.Height, FPS: 30,
	}
	if err := b.OpenSlack(ctx, w.cfg.CookieD, shim, DeepLinkURL(w.cfg.TeamID, w.cfg.ChannelID)); err != nil {
		b.SaveDebug(w.cfg.DebugDir, "open-"+w.cfg.ChannelID)
		return err
	}
	d := &Driver{B: b, Log: w.cfg.Log}

	for {
		w.setStatus(StatusJoining, "")
		if err := d.Join(ctx, w.cfg.TeamID, w.cfg.ChannelID, w.cfg.JoinTimeout); err != nil {
			b.SaveDebug(w.cfg.DebugDir, "join-"+w.cfg.ChannelID)
			return err
		}
		b.AdoptHuddleWindow()
		w.setStatus(StatusJoined, "")
		time.Sleep(2 * time.Second)
		d.EnsureUnmuted()
		if err := w.apply(ctx, b, d, w.Snapshot().Presentation); err != nil {
			b.SaveDebug(w.cfg.DebugDir, "present-"+w.cfg.ChannelID)
			w.setStatus(StatusJoined, err.Error())
		} else {
			w.setStatus(StatusStreaming, "")
		}

		again, err := w.monitor(ctx, b, d)
		if err != nil || !again {
			return err
		}
		if err := b.Page().Navigate(DeepLinkURL(w.cfg.TeamID, w.cfg.ChannelID)); err != nil {
			return err
		}
		_ = b.Page().Timeout(30 * time.Second).WaitLoad()
		if err := b.CheckLoggedIn(); err != nil {
			return err
		}
	}
}

func (w *Worker) apply(ctx context.Context, b *browser.Browser, d *Driver, p string) error {
	if err := b.SetPresentation(p); err != nil {
		return err
	}
	var errs []error
	switch p {
	case PresentationScreen:
		errs = append(errs, d.StartShare(ctx), d.SetCamera(ctx, false))
	case PresentationCamera:
		d.StopShare()
		errs = append(errs, d.SetCamera(ctx, true))
	case PresentationBoth:
		errs = append(errs, d.StartShare(ctx), d.SetCamera(ctx, true))
	}
	return errors.Join(errs...)
}

func (w *Worker) monitor(ctx context.Context, b *browser.Browser, d *Driver) (rejoin bool, err error) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var aloneSince, lastShot, lastShareRetry time.Time
	for {
		select {
		case <-ctx.Done():
			d.Leave()
			return false, nil
		case p := <-w.presCh:
			if err := w.apply(ctx, b, d, p); err != nil {
				w.setStatus(StatusJoined, err.Error())
			} else {
				w.setStatus(StatusStreaming, "")
			}
		case <-tick.C:
		}

		if err := b.CheckLoggedIn(); err != nil {
			return false, err
		}
		b.AdoptHuddleWindow()
		if !d.InHuddle() {
			b.SaveDebug(w.cfg.DebugDir, "dropped-"+w.cfg.ChannelID)
			return false, errors.New("dropped out of the huddle")
		}

		shim, _ := b.ShimState()
		n := d.Participants()
		w.mu.Lock()
		w.snap.Shim = shim
		w.snap.Participants = n
		w.snap.URL = b.URL()
		pres := w.snap.Presentation
		w.mu.Unlock()

		sourceOK := shim == nil || shim.Status == "live"
		if !sourceOK {
			w.setStatus(StatusJoined, "waiting for source: "+shim.Error)
		} else if w.Snapshot().Status == StatusJoined && time.Since(lastShareRetry) > 30*time.Second {
			lastShareRetry = time.Now()
			if err := w.apply(ctx, b, d, pres); err == nil {
				w.setStatus(StatusStreaming, "")
			} else {
				w.setStatus(StatusJoined, err.Error())
			}
		}
		if sourceOK && (pres == PresentationScreen || pres == PresentationBoth) && !d.IsSharing() && time.Since(lastShareRetry) > 30*time.Second {
			lastShareRetry = time.Now()
			if err := d.StartShare(ctx); err != nil {
				w.setStatus(StatusJoined, err.Error())
			} else {
				w.setStatus(StatusStreaming, "")
			}
		}

		if time.Since(lastShot) >= w.cfg.ScreenshotInterval {
			lastShot = time.Now()
			if img, err := b.Screenshot(); err == nil {
				w.mu.Lock()
				w.snap.Screenshot, w.snap.ScreenshotAt = img, lastShot
				w.mu.Unlock()
			}
		}

		if w.cfg.LeaveWhenAloneAfter > 0 {
			if n == 1 {
				if aloneSince.IsZero() {
					aloneSince = time.Now()
				} else if time.Since(aloneSince) > w.cfg.LeaveWhenAloneAfter {
					w.cfg.Log.Info("alone in huddle, leaving")
					d.Leave()
					w.setStatus(StatusWaiting, "left: nobody else in the huddle")
					select {
					case <-ctx.Done():
						return false, nil
					case <-time.After(3 * time.Minute):
					}
					return true, nil
				}
			} else {
				aloneSince = time.Time{}
			}
		}
	}
}
