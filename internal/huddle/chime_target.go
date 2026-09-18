package huddle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/LeafdTK/huddlecast/internal/browser"
	"github.com/LeafdTK/huddlecast/internal/slackrooms"
)

type chimePageState struct {
	Status       string `json:"status"`
	Error        string `json:"error"`
	Joined       bool   `json:"joined"`
	Audio        bool   `json:"audio"`
	AudioProfile string `json:"audioProfile"`
	ContentShare bool   `json:"contentShare"`
	WhepConnects int    `json:"whepConnects"`
	MicLevel     int    `json:"micLevel"`
	MicMuted     bool   `json:"micMuted"`
	PcState      string `json:"pcState"`
	DropReason   string `json:"dropReason"`
	Tracks       string `json:"tracks"`
	Standby       bool   `json:"standby"`
	SrcRes        string `json:"srcRes"`
	SrcFps        int    `json:"srcFps"`
	SrcKbps       int    `json:"srcKbps"`
	PaintFps      int    `json:"paintFps"`
	TargetFps     int    `json:"targetFps"`
	FramesDropped int    `json:"framesDropped"`
	DropRate      int    `json:"dropRate"`
}

type ChimeTarget struct {
	cfg  Config
	mu   sync.Mutex
	snap Snapshot
}

func newChimeTarget(cfg Config) *ChimeTarget {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cfg.Log = cfg.Log.With("chime-target", cfg.ChannelName, "account", cfg.AccountName)
	return &ChimeTarget{cfg: cfg, snap: Snapshot{
		Status: StatusPending, Participants: -1, Account: cfg.AccountName,
		ChannelID: cfg.ChannelID, ChannelName: cfg.ChannelName, Presentation: cfg.Presentation, Since: time.Now(),
	}}
}

func (t *ChimeTarget) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snap
}

func (t *ChimeTarget) SetPresentation(p string) {
	t.mu.Lock()
	t.snap.Presentation = p
	t.mu.Unlock()
}

func (t *ChimeTarget) setStatus(s Status, err string) {
	t.mu.Lock()
	changed := t.snap.Status != s || t.snap.Error != err
	t.snap.Status, t.snap.Error = s, err
	if changed {
		t.snap.Since = time.Now()
	}
	t.mu.Unlock()
	if changed {
		t.cfg.Log.Info("chime target status", "status", s, "err", err)
		if t.cfg.OnStatus != nil {
			t.cfg.OnStatus(s, err)
		}
	}
}

func (t *ChimeTarget) Run(ctx context.Context) error {
	backoff := 2 * time.Second
	for {
		err := t.runOnce(ctx)
		if ctx.Err() != nil {
			t.setStatus(StatusStopped, "")
			return nil
		}
		wait := backoff
		switch {
		case err == nil:
			wait = 2 * time.Second
		case errors.Is(err, slackrooms.ErrNotLoggedIn):
			t.setStatus(StatusNeedsReauth, err.Error())
			wait = 5 * time.Minute
		default:
			t.setStatus(StatusError, err.Error())
			backoff = min(backoff*2, t.cfg.RejoinBackoffMax)
		}
		select {
		case <-ctx.Done():
			t.setStatus(StatusStopped, "")
			return nil
		case <-time.After(wait):
		}
	}
}

func (t *ChimeTarget) initScript(join *slackrooms.Join) (string, error) {
	c := map[string]any{
		"mode":         "send",
		"meeting":      json.RawMessage(join.Meeting),
		"attendee":     json.RawMessage(join.Attendee),
		"whepUrl":      t.cfg.WHEPURL,
		"width":        t.cfg.VideoWidth,
		"height":       t.cfg.VideoHeight,
		"fps":          t.cfg.VideoFPS,
		"videoMaxKbps": t.cfg.VideoMaxKbps,
		"audioProfile": t.cfg.AudioProfile,
		"audioMaxKbps": t.cfg.AudioMaxKbps,
		"debug":        true,
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("window.__HUDDLECAST_CHIME_CFG = %s;", b), nil
}

func (t *ChimeTarget) runOnce(ctx context.Context) error {
	if t.cfg.Host == "" {
		return errors.New("chime target needs slack.host")
	}
	if t.cfg.PageURL == "" {
		return errors.New("chime target needs a page url")
	}
	t.setStatus(StatusLaunching, "")
	cli := slackrooms.New(t.cfg.Host, t.cfg.CookieD)
	join, err := cli.JoinRoom(ctx, t.cfg.ChannelID, t.cfg.Region)
	if err != nil {
		return fmt.Errorf("rooms.join: %w", err)
	}
	t.cfg.Log.Info("joined chime meeting", "meeting", join.MeetingID, "attendee", join.AttendeeID)
	script, err := t.initScript(join)
	if err != nil {
		return err
	}
	opts := t.cfg.Browser
	opts.Log = t.cfg.Log
	b, err := browser.Launch(ctx, opts)
	if err != nil {
		return err
	}
	defer b.Close()
	t.setStatus(StatusJoining, "")
	if err := b.OpenPage(ctx, t.cfg.PageURL, script); err != nil {
		b.SaveDebug(t.cfg.DebugDir, "chime-target-"+t.cfg.ChannelID)
		return err
	}

	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var lastShot, lastInfo time.Time
	var lastSt chimePageState
	var lastMic int
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		var st chimePageState
		_ = b.Eval(`() => window.__huddlecastChime ? window.__huddlecastChime.snapshot() : null`, &st)
		sig := chimePageState{Status: st.Status, Error: st.Error, Audio: st.Audio, AudioProfile: st.AudioProfile, ContentShare: st.ContentShare, MicMuted: st.MicMuted, PcState: st.PcState, DropReason: st.DropReason, Tracks: st.Tracks}
		if sig != lastSt || (st.MicLevel > 0) != (lastMic > 0) {
			lastSt, lastMic = sig, st.MicLevel
			t.cfg.Log.Info("chime page", "status", st.Status, "share", st.ContentShare, "audio", st.Audio, "tracks", st.Tracks, "pc", st.PcState, "drop", st.DropReason, "micLevel", st.MicLevel, "whep", st.WhepConnects, "err", st.Error)
		}
		t.mu.Lock()
		t.snap.Shim = &browser.ShimState{Status: st.Status, Error: st.Error, WHEPConnects: st.WhepConnects}
		t.snap.Stats = &Stats{SourceRes: st.SrcRes, SourceFPS: st.SrcFps, SourceKbps: st.SrcKbps, PaintFPS: st.PaintFps, TargetFPS: st.TargetFps, FramesDropped: st.FramesDropped, DropRate: st.DropRate, Standby: st.Standby, MicLevel: st.MicLevel}
		t.mu.Unlock()
		switch st.Status {
		case "live":
			t.setStatus(StatusStreaming, "")
		case "error":
			t.setStatus(StatusJoined, st.Error)
		case "joined", "joining":
			t.setStatus(StatusJoined, st.Error)
		}
		if time.Since(lastInfo) >= 10*time.Second {
			lastInfo = time.Now()
			if info, err := cli.RoomInfo(ctx, join.CallID); err == nil {
				t.mu.Lock()
				t.snap.Participants = len(info.Participants)
				t.mu.Unlock()
				if info.HasEnded {
					return errors.New("huddle ended")
				}
			}
		}
		if time.Since(lastShot) >= t.cfg.ScreenshotInterval {
			lastShot = time.Now()
			if img, err := b.Screenshot(); err == nil {
				t.mu.Lock()
				t.snap.Screenshot, t.snap.ScreenshotAt = img, lastShot
				t.mu.Unlock()
			}
		}
	}
}
