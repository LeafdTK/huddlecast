package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LeafdTK/huddlecast/internal/browser"
	"github.com/LeafdTK/huddlecast/internal/config"
	"github.com/LeafdTK/huddlecast/internal/huddle"
	"github.com/LeafdTK/huddlecast/internal/huddle/mirror"
	"github.com/LeafdTK/huddlecast/internal/media"
	"github.com/LeafdTK/huddlecast/internal/store"
)

const (
	SourcePush   = "push"
	SourceMirror = "mirror"
)

type Channel struct {
	ID   string
	Name string
}

type StartRequest struct {
	CreatedBy    string
	SourceType   string
	SourceRef    string
	Presentation string
	Targets      []Channel
	Quality      *config.Quality
	Record       bool
}

type TargetView struct {
	store.Target
	Snap *huddle.Snapshot
}

type SessionView struct {
	store.Session
	Targets    []TargetView
	Mirror     *mirror.Snapshot
	SourcePath string
	SourceLive bool
}

type targetRun struct {
	t      store.Target
	w      huddle.TargetEngine
	cancel context.CancelFunc
	done   chan struct{}
}

type runtime struct {
	sess         store.Session
	ctx          context.Context
	cancel       context.CancelFunc
	targets      map[int64]*targetRun
	quality      config.Quality
	mirror       mirror.Engine
	mirrorCancel context.CancelFunc
	mirrorDone   chan struct{}
}

type Manager struct {
	cfg         *config.Config
	store       *store.Store
	media       *media.Client
	log         *slog.Logger
	events      *Broker
	mirrorToken string
	baseCtx     context.Context

	mu       sync.Mutex
	sessions map[string]*runtime
}

func New(ctx context.Context, cfg *config.Config, st *store.Store, m *media.Client, log *slog.Logger) *Manager {
	tok := make([]byte, 24)
	_, _ = rand.Read(tok)
	return &Manager{
		cfg: cfg, store: st, media: m, log: log, events: NewBroker(),
		mirrorToken: hex.EncodeToString(tok), baseCtx: ctx, sessions: map[string]*runtime{},
	}
}

func (m *Manager) Events() *Broker      { return m.events }
func (m *Manager) MirrorToken() string  { return m.mirrorToken }
func (m *Manager) Media() *media.Client { return m.media }

func (m *Manager) StreamKeyExists(ctx context.Context, key string) (bool, error) {
	k, err := m.store.GetStreamKey(ctx, key)
	return k != nil, err
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102") + "-" + hex.EncodeToString(b)
}

func sourcePath(s store.Session) string {
	if s.SourceType == SourceMirror {
		return media.MirrorPath(s.ID)
	}
	return media.PushPath(s.SourceRef)
}

func (m *Manager) Resume(ctx context.Context) error {
	sessions, err := m.store.ListSessions(ctx, true, 100)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		targets, err := m.store.ListTargets(ctx, s.ID)
		if err != nil {
			return err
		}
		if err := m.media.SetRecord(ctx, sourcePath(s), s.Record); err != nil {
			m.log.Warn("set record", "session", s.ID, "err", err)
		}
		m.mu.Lock()
		rt := m.newRuntime(s)
		m.sessions[s.ID] = rt
		if s.SourceType == SourceMirror {
			m.startMirror(rt, s.SourceRef)
		}
		for _, t := range targets {
			m.startTarget(rt, t)
		}
		m.mu.Unlock()
		m.log.Info("resumed session", "id", s.ID, "targets", len(targets))
	}
	return nil
}

func (m *Manager) newRuntime(s store.Session) *runtime {
	ctx, cancel := context.WithCancel(m.baseCtx)
	return &runtime{sess: s, ctx: ctx, cancel: cancel, targets: map[int64]*targetRun{}, quality: m.cfg.Quality}
}

func (m *Manager) Start(ctx context.Context, req StartRequest) (*store.Session, error) {
	if !huddle.ValidPresentation(req.Presentation) {
		return nil, fmt.Errorf("invalid presentation %q", req.Presentation)
	}
	if len(req.Targets) == 0 {
		return nil, errors.New("at least one target channel is required")
	}
	switch req.SourceType {
	case SourcePush:
		ok, err := m.StreamKeyExists(ctx, req.SourceRef)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unknown stream key %q", req.SourceRef)
		}
	case SourceMirror:
		if req.SourceRef == "" {
			return nil, errors.New("mirror source needs a channel id")
		}
		for _, t := range req.Targets {
			if t.ID == req.SourceRef {
				return nil, errors.New("source channel cannot also be a target")
			}
		}
	default:
		return nil, fmt.Errorf("invalid source type %q", req.SourceType)
	}
	sess := store.Session{
		ID: newID(), CreatedBy: req.CreatedBy, SourceType: req.SourceType, SourceRef: req.SourceRef,
		Presentation: req.Presentation, Status: "running", Record: req.Record,
	}
	if err := m.store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	if err := m.media.SetRecord(ctx, sourcePath(sess), sess.Record); err != nil {
		m.log.Warn("set record", "session", sess.ID, "err", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.newRuntime(sess)
	if req.Quality != nil {
		rt.quality = *req.Quality
	}
	m.sessions[sess.ID] = rt
	if sess.SourceType == SourceMirror {
		m.startMirror(rt, sess.SourceRef)
	}
	for _, ch := range req.Targets {
		if _, err := m.addTargetLocked(ctx, rt, ch); err != nil {
			m.log.Error("add target", "channel", ch.ID, "err", err)
		}
	}
	m.events.Publish(Event{Type: "session", SessionID: sess.ID})
	return &sess, nil
}

func (m *Manager) identities() []config.Account {
	out := append([]config.Account(nil), m.cfg.Accounts...)
	seen := map[string]bool{}
	for _, a := range out {
		seen[a.Name] = true
	}
	ids, err := m.store.ListIdentities(m.baseCtx)
	if err != nil {
		m.log.Warn("list identities", "err", err)
	}
	for _, id := range ids {
		if seen[id.Name] {
			for i := range out {
				if out[i].Name == id.Name && id.CookieD != "" {
					out[i].CookieD = id.CookieD
				}
			}
			continue
		}
		out = append(out, config.Account{Name: id.Name, CookieD: id.CookieD})
	}
	return out
}

func (m *Manager) account(name string) (config.Account, bool) {
	for _, a := range m.identities() {
		if a.Name == name {
			return a, true
		}
	}
	return config.Account{}, false
}

func (m *Manager) pickAccountLocked() config.Account {
	accts := m.identities()
	if len(accts) == 0 {
		return config.Account{}
	}
	load := map[string]int{}
	for _, a := range accts {
		load[a.Name] = 0
	}
	for _, rt := range m.sessions {
		for _, tr := range rt.targets {
			load[tr.t.Account]++
		}
		if rt.mirror != nil {
			load[rt.mirror.Snapshot().Account]++
		}
	}
	best := accts[0]
	for _, a := range accts[1:] {
		if load[a.Name] < load[best.Name] {
			best = a
		}
	}
	return best
}

func (m *Manager) chimePageURL() string {
	addr := m.cfg.Listen
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr + "/static/chime/mirror.html"
}

func (m *Manager) browserOpts(profile string) browser.Options {
	return browser.Options{
		Bin: m.cfg.Browser.Bin, Headless: m.cfg.Browser.Headless,
		ProfileDir: filepath.Join(m.cfg.DataDir, "profiles", profile),
		Width:      m.cfg.Browser.Width, Height: m.cfg.Browser.Height, Log: m.log,
	}
}

func (m *Manager) addTargetLocked(ctx context.Context, rt *runtime, ch Channel) (*targetRun, error) {
	for _, tr := range rt.targets {
		if tr.t.ChannelID == ch.ID {
			return nil, fmt.Errorf("channel %s is already a target", ch.ID)
		}
	}
	acct := m.pickAccountLocked()
	t := store.Target{SessionID: rt.sess.ID, ChannelID: ch.ID, ChannelName: ch.Name, Account: acct.Name, Status: string(huddle.StatusPending)}
	id, err := m.store.AddTarget(ctx, t)
	if err != nil {
		return nil, err
	}
	t.ID = id
	return m.startTarget(rt, t), nil
}

func (m *Manager) startTarget(rt *runtime, t store.Target) *targetRun {
	acct, _ := m.account(t.Account)
	tctx, cancel := context.WithCancel(rt.ctx)
	tr := &targetRun{t: t, cancel: cancel, done: make(chan struct{})}
	q := rt.quality
	bopts := m.browserOpts(fmt.Sprintf("%s-%s", acct.Name, t.ChannelID))
	if q.Video.Width > 0 {
		bopts.Width = q.Video.Width
	}
	if q.Video.Height > 0 {
		bopts.Height = q.Video.Height
	}
	tr.w = huddle.NewTargetEngine(huddle.Config{
		Engine: m.cfg.Huddle.TargetEngine, Host: m.cfg.Slack.Host, PageURL: m.chimePageURL(),
		VideoWidth: q.Video.Width, VideoHeight: q.Video.Height, VideoFPS: q.Video.FPS,
		VideoMaxKbps: q.Video.MaxBitrateKbps, AudioProfile: q.Audio.Profile, AudioMaxKbps: q.Audio.MaxBitrateKbps,
		TeamID: m.cfg.Slack.TeamID, ChannelID: t.ChannelID, ChannelName: t.ChannelName,
		AccountName: acct.Name, CookieD: acct.CookieD,
		WHEPURL:      m.media.WHEPURL(sourcePath(rt.sess)),
		Presentation: rt.sess.Presentation,
		Browser:      bopts,
		DebugDir:     filepath.Join(m.cfg.DataDir, "debug"),
		JoinTimeout:  m.cfg.Huddle.JoinTimeout, LeaveWhenAloneAfter: m.cfg.Huddle.LeaveWhenAloneAfter,
		RejoinBackoffMax: m.cfg.Huddle.RejoinBackoffMax, ScreenshotInterval: m.cfg.Browser.ScreenshotInterval,
		Log: m.log,
		OnStatus: func(s huddle.Status, e string) {
			_ = m.store.UpdateTargetStatus(m.baseCtx, t.ID, string(s), e)
			if s == huddle.StatusNeedsReauth {
				_ = m.store.SetAccountState(m.baseCtx, acct.Name, "needs_reauth", e)
			} else if s == huddle.StatusStreaming {
				_ = m.store.SetAccountState(m.baseCtx, acct.Name, "ok", "")
			}
			m.events.Publish(Event{Type: "target", SessionID: rt.sess.ID, Data: map[string]any{"targetId": t.ID, "status": s, "error": e}})
		},
	})
	rt.targets[t.ID] = tr
	go func() {
		defer close(tr.done)
		_ = tr.w.Run(tctx)
	}()
	return tr
}

func (m *Manager) startMirror(rt *runtime, channelID string) {
	acct := m.pickAccountLocked()
	mctx, cancel := context.WithCancel(rt.ctx)
	var targetUsers []string
	if wl, err := m.store.ListWhitelist(m.baseCtx); err == nil {
		for _, e := range wl {
			targetUsers = append(targetUsers, e.UserID)
		}
	}
	q := rt.quality
	bopts := m.browserOpts(fmt.Sprintf("%s-mirror-%s", acct.Name, channelID))
	if q.Video.Width > 0 {
		bopts.Width = q.Video.Width
	}
	if q.Video.Height > 0 {
		bopts.Height = q.Video.Height
	}
	w := mirror.NewEngine(mirror.Config{
		Engine: m.cfg.Huddle.MirrorEngine,
		Host:   m.cfg.Slack.Host, TargetUsers: targetUsers, PageURL: m.chimePageURL(),
		VideoWidth: q.Video.Width, VideoHeight: q.Video.Height, VideoFPS: q.Video.FPS,
		TeamID: m.cfg.Slack.TeamID, ChannelID: channelID, ChannelName: channelID,
		AccountName: acct.Name, CookieD: acct.CookieD,
		WHIPURL: m.media.WHIPURL(media.MirrorPath(rt.sess.ID)), Token: m.mirrorToken,
		Browser:     bopts,
		DebugDir:    filepath.Join(m.cfg.DataDir, "debug"),
		JoinTimeout: m.cfg.Huddle.JoinTimeout, RejoinBackoffMax: m.cfg.Huddle.RejoinBackoffMax,
		ScreenshotInterval: m.cfg.Browser.ScreenshotInterval, Log: m.log,
		OnStatus: func(s huddle.Status, e string) {
			if s == huddle.StatusNeedsReauth {
				_ = m.store.SetAccountState(m.baseCtx, acct.Name, "needs_reauth", e)
			}
			m.events.Publish(Event{Type: "mirror", SessionID: rt.sess.ID, Data: map[string]any{"status": s, "error": e}})
		},
	})
	rt.mirror, rt.mirrorCancel, rt.mirrorDone = w, cancel, make(chan struct{})
	go func() {
		defer close(rt.mirrorDone)
		_ = w.Run(mctx)
	}()
}

func (m *Manager) AddTarget(ctx context.Context, sessionID string, ch Channel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		return errors.New("session not running")
	}
	if rt.sess.SourceType == SourceMirror && rt.sess.SourceRef == ch.ID {
		return errors.New("source channel cannot also be a target")
	}
	_, err := m.addTargetLocked(ctx, rt, ch)
	if err == nil {
		m.events.Publish(Event{Type: "session", SessionID: sessionID})
	}
	return err
}

func (m *Manager) RemoveTarget(ctx context.Context, sessionID string, targetID int64) error {
	m.mu.Lock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return errors.New("session not running")
	}
	tr, ok := rt.targets[targetID]
	if !ok {
		m.mu.Unlock()
		return errors.New("target not found")
	}
	delete(rt.targets, targetID)
	m.mu.Unlock()
	tr.cancel()
	select {
	case <-tr.done:
	case <-time.After(15 * time.Second):
	}
	if err := m.store.DeleteTarget(ctx, targetID); err != nil {
		return err
	}
	m.events.Publish(Event{Type: "session", SessionID: sessionID})
	return nil
}

func (m *Manager) RemoveTargetByChannel(ctx context.Context, sessionID, channelID string) error {
	m.mu.Lock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return errors.New("session not running")
	}
	var id int64 = -1
	for _, tr := range rt.targets {
		if tr.t.ChannelID == channelID {
			id = tr.t.ID
		}
	}
	m.mu.Unlock()
	if id < 0 {
		return errors.New("channel is not a target of this session")
	}
	return m.RemoveTarget(ctx, sessionID, id)
}

func (m *Manager) SetPresentation(ctx context.Context, sessionID, p string) error {
	if !huddle.ValidPresentation(p) {
		return fmt.Errorf("invalid presentation %q", p)
	}
	m.mu.Lock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return errors.New("session not running")
	}
	rt.sess.Presentation = p
	for _, tr := range rt.targets {
		tr.w.SetPresentation(p)
	}
	m.mu.Unlock()
	if err := m.store.UpdateSession(ctx, sessionID, p, "running"); err != nil {
		return err
	}
	m.events.Publish(Event{Type: "session", SessionID: sessionID})
	return nil
}

func (m *Manager) SetRecord(ctx context.Context, sessionID string, record bool) error {
	m.mu.Lock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return errors.New("session not running")
	}
	rt.sess.Record = record
	path := sourcePath(rt.sess)
	m.mu.Unlock()
	if err := m.store.SetSessionRecord(ctx, sessionID, record); err != nil {
		return err
	}
	if err := m.media.SetRecord(ctx, path, record); err != nil {
		return err
	}
	m.events.Publish(Event{Type: "session", SessionID: sessionID})
	return nil
}

func (m *Manager) Recording(sessionID string) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		return false, false
	}
	return rt.sess.Record, true
}

func (m *Manager) Quality(sessionID string) (config.Quality, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		return config.Quality{}, false
	}
	return rt.quality, true
}

func (m *Manager) SetQuality(sessionID string, q config.Quality) error {
	m.mu.Lock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return errors.New("session not running")
	}
	rt.quality = q
	var targs []store.Target
	old := rt.targets
	for _, tr := range old {
		targs = append(targs, tr.t)
		tr.cancel()
	}
	rt.targets = map[int64]*targetRun{}
	mcancel, mdone := rt.mirrorCancel, rt.mirrorDone
	hadMirror := rt.mirror != nil
	mirrorCh := rt.sess.SourceRef
	rt.mirror, rt.mirrorCancel, rt.mirrorDone = nil, nil, nil
	m.mu.Unlock()

	for _, tr := range old {
		select {
		case <-tr.done:
		case <-time.After(15 * time.Second):
		}
	}
	if hadMirror && mcancel != nil {
		mcancel()
		select {
		case <-mdone:
		case <-time.After(15 * time.Second):
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok = m.sessions[sessionID]
	if !ok {
		return nil
	}
	if hadMirror && rt.sess.SourceType == SourceMirror {
		m.startMirror(rt, mirrorCh)
	}
	for _, t := range targs {
		m.startTarget(rt, t)
	}
	m.events.Publish(Event{Type: "session", SessionID: sessionID})
	return nil
}

func (m *Manager) Stop(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	rt, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()
	if !ok {
		return errors.New("session not running")
	}
	rt.cancel()
	deadline := time.After(20 * time.Second)
	for _, tr := range rt.targets {
		select {
		case <-tr.done:
		case <-deadline:
		}
	}
	if rt.mirrorDone != nil {
		select {
		case <-rt.mirrorDone:
		case <-deadline:
		}
	}
	if err := m.store.UpdateSession(ctx, sessionID, rt.sess.Presentation, "stopped"); err != nil {
		return err
	}
	m.events.Publish(Event{Type: "session", SessionID: sessionID})
	return nil
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.mu.Lock()
		rt := m.sessions[id]
		m.mu.Unlock()
		if rt == nil {
			continue
		}
		rt.cancel()
		for _, tr := range rt.targets {
			select {
			case <-tr.done:
			case <-time.After(10 * time.Second):
			}
		}
	}
}

func (m *Manager) Running() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) SessionForChannel(channelID string) (sessionID string, isTarget bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, rt := range m.sessions {
		for _, tr := range rt.targets {
			if tr.t.ChannelID == channelID {
				return id, true
			}
		}
	}
	return "", false
}

func (m *Manager) View(ctx context.Context, sessionID string) (*SessionView, error) {
	sess, err := m.store.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	targets, err := m.store.ListTargets(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	v := &SessionView{Session: *sess, SourcePath: sourcePath(*sess)}
	m.mu.Lock()
	rt := m.sessions[sessionID]
	if rt != nil {
		v.Session.Presentation = rt.sess.Presentation
	}
	for _, t := range targets {
		tv := TargetView{Target: t}
		if rt != nil {
			if tr, ok := rt.targets[t.ID]; ok {
				s := tr.w.Snapshot()
				tv.Snap = &s
			}
		}
		v.Targets = append(v.Targets, tv)
	}
	if rt != nil && rt.mirror != nil {
		s := rt.mirror.Snapshot()
		v.Mirror = &s
	}
	m.mu.Unlock()
	if p, err := m.media.GetPath(ctx, v.SourcePath); err == nil && p != nil {
		v.SourceLive = p.IsLive()
	}
	return v, nil
}

func (m *Manager) ListViews(ctx context.Context, onlyRunning bool, limit int) ([]SessionView, error) {
	sessions, err := m.store.ListSessions(ctx, onlyRunning, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SessionView, 0, len(sessions))
	for _, s := range sessions {
		v, err := m.View(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		if v != nil {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (m *Manager) Screenshot(sessionID string, targetID int64) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.sessions[sessionID]
	if !ok {
		return nil
	}
	if targetID < 0 {
		if rt.mirror != nil {
			return rt.mirror.Snapshot().Screenshot
		}
		return nil
	}
	if tr, ok := rt.targets[targetID]; ok {
		return tr.w.Snapshot().Screenshot
	}
	return nil
}
