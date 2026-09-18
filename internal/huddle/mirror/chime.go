package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/LeafdTK/huddlecast/internal/browser"
	"github.com/LeafdTK/huddlecast/internal/huddle"
	"github.com/LeafdTK/huddlecast/internal/slackrooms"
)

type ChimeWorker struct {
	cfg  Config
	mu   sync.Mutex
	snap Snapshot
}

func newChime(cfg Config) *ChimeWorker {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.Log = cfg.Log.With("chime-mirror", cfg.ChannelName, "account", cfg.AccountName)
	return &ChimeWorker{cfg: cfg, snap: Snapshot{
		Status: huddle.StatusPending, Participants: -1, Account: cfg.AccountName,
		ChannelID: cfg.ChannelID, ChannelName: cfg.ChannelName, Since: time.Now(),
	}}
}

func (w *ChimeWorker) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snap
}

func (w *ChimeWorker) setStatus(s huddle.Status, err string) {
	w.mu.Lock()
	changed := w.snap.Status != s || w.snap.Error != err
	w.snap.Status, w.snap.Error = s, err
	if changed {
		w.snap.Since = time.Now()
	}
	w.mu.Unlock()
	if changed {
		w.cfg.Log.Info("chime mirror status", "status", s, "err", err)
		if w.cfg.OnStatus != nil {
			w.cfg.OnStatus(s, err)
		}
	}
}

func (w *ChimeWorker) Run(ctx context.Context) error {
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
		case errors.Is(err, slackrooms.ErrNotLoggedIn):
			w.setStatus(huddle.StatusNeedsReauth, err.Error())
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

func (w *ChimeWorker) initScript(join *slackrooms.Join) (string, error) {
	cfg := map[string]any{
		"meeting":       json.RawMessage(join.Meeting),
		"attendee":      json.RawMessage(join.Attendee),
		"targetUserIDs": w.cfg.TargetUsers,
		"kind":          "",
		"whipUrl":       w.cfg.WHIPURL,
		"token":         w.cfg.Token,
		"width":         w.cfg.VideoWidth,
		"height":        w.cfg.VideoHeight,
		"fps":           w.cfg.VideoFPS,
		"debug":         true,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("window.__HUDDLECAST_CHIME_CFG = %s;", b), nil
}

func (w *ChimeWorker) runOnce(ctx context.Context) error {
	if w.cfg.Host == "" {
		return errors.New("chime mirror needs slack.host (the workspace/enterprise host)")
	}
	if w.cfg.PageURL == "" {
		return errors.New("chime mirror needs a page url")
	}
	w.setStatus(huddle.StatusLaunching, "")
	cli := slackrooms.New(w.cfg.Host, w.cfg.CookieD)
	join, err := cli.JoinRoom(ctx, w.cfg.ChannelID, w.cfg.Region)
	if err != nil {
		return fmt.Errorf("rooms.join: %w", err)
	}
	w.cfg.Log.Info("joined chime meeting", "meeting", join.MeetingID, "attendee", join.AttendeeID)

	script, err := w.initScript(join)
	if err != nil {
		return err
	}
	opts := w.cfg.Browser
	opts.Log = w.cfg.Log
	b, err := browser.Launch(ctx, opts)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := b.OpenPage(ctx, w.cfg.PageURL, script); err != nil {
		b.SaveDebug(w.cfg.DebugDir, "chime-open-"+w.cfg.ChannelID)
		return err
	}
	w.setStatus(huddle.StatusJoined, "")

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var lastShot, lastInfo time.Time
	var lastSeen string
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		var st State
		_ = b.Eval(`() => window.__huddlecastChime ? window.__huddlecastChime.snapshot() : null`, &st)
		if st.Seen != lastSeen {
			lastSeen = st.Seen
			w.cfg.Log.Info("chime mirror tiles", "tiles", st.Tiles, "targets", st.Targets, "bound", st.TargetUser, "source", st.SourceVideo, "seen", st.Seen)
		}
		w.mu.Lock()
		w.snap.Mirror = &st
		w.mu.Unlock()
		switch st.Status {
		case "live":
			w.setStatus(huddle.StatusStreaming, "")
		case "error":
			w.setStatus(huddle.StatusJoined, st.Error)
		}
		if time.Since(lastInfo) >= 10*time.Second {
			lastInfo = time.Now()
			if info, err := cli.RoomInfo(ctx, join.CallID); err == nil {
				w.mu.Lock()
				w.snap.Participants = len(info.Participants)
				w.mu.Unlock()
				if info.HasEnded {
					return errors.New("source huddle ended")
				}
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
	}
}
