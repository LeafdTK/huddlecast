package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LeafdTK/huddlecast/internal/config"
	"github.com/LeafdTK/huddlecast/internal/huddle"
	"github.com/LeafdTK/huddlecast/internal/media"
	"github.com/LeafdTK/huddlecast/internal/recording"
	"github.com/LeafdTK/huddlecast/internal/session"
	"github.com/LeafdTK/huddlecast/internal/slackapp"
	"github.com/LeafdTK/huddlecast/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

var assetVer = strconv.FormatInt(time.Now().Unix(), 10)

type Server struct {
	cfg   *config.Config
	store *store.Store
	mgr   *session.Manager
	slack *slackapp.App
	media *media.Client
	rec   *recording.Uploader
	log   *slog.Logger
	tmpl  map[string]*template.Template
	mux   *http.ServeMux
}

func New(cfg *config.Config, st *store.Store, mgr *session.Manager, sl *slackapp.App, m *media.Client, rec *recording.Uploader, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, store: st, mgr: mgr, slack: sl, media: m, rec: rec, log: log, tmpl: map[string]*template.Template{}, mux: http.NewServeMux()}
	funcs := template.FuncMap{
		"since": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return time.Since(t).Truncate(time.Second).String()
		},
		"ago": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			return time.Since(t).Truncate(time.Second).String() + " ago"
		},
		"bytes": func(n int64) string {
			switch {
			case n > 1<<30:
				return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
			case n > 1<<20:
				return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
			}
			return fmt.Sprintf("%d KB", n/1024)
		},
	}
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		name := filepath.Base(p)
		if name == "layout.html" {
			continue
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(assets, "templates/layout.html", p)
		if err != nil {
			return nil, err
		}
		s.tmpl[name] = t
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	static, _ := fs.Sub(assets, "static")
	staticFS := http.StripPrefix("/static/", http.FileServer(http.FS(static)))
	s.mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".bundle.js") || strings.HasSuffix(r.URL.Path, "primer.css") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		staticFS.ServeHTTP(w, r)
	}))
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	s.mux.Handle("POST /hooks/mediamtx/auth", media.AuthHandler(s.mgr, s.mgr.MirrorToken(), s.log))

	s.mux.HandleFunc("GET /login", s.handleLogin)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("GET /logout", s.handleLogout)
	s.mux.HandleFunc("GET /auth/slack", s.handleSlackAuth)
	s.mux.HandleFunc("GET /auth/callback", s.handleSlackCallback)

	auth := func(f http.HandlerFunc) http.Handler { return s.requireAuth(f) }
	admin := func(f http.HandlerFunc) http.Handler { return s.requireAdmin(f) }
	s.mux.Handle("GET /{$}", auth(s.handleDashboard))
	s.mux.Handle("GET /sessions/new", auth(s.handleNewSession))
	s.mux.Handle("POST /sessions", auth(s.handleStartSession))
	s.mux.Handle("GET /sessions/{id}", auth(s.handleSession))
	s.mux.Handle("POST /sessions/{id}/stop", auth(s.handleStopSession))
	s.mux.Handle("POST /sessions/{id}/targets", auth(s.handleAddTarget))
	s.mux.Handle("POST /sessions/{id}/targets/{tid}/remove", auth(s.handleRemoveTarget))
	s.mux.Handle("POST /sessions/{id}/presentation", auth(s.handlePresentation))
	s.mux.Handle("POST /sessions/{id}/quality", auth(s.handleQuality))
	s.mux.Handle("POST /sessions/{id}/record", auth(s.handleRecordToggle))
	s.mux.Handle("GET /sessions/{id}/stats.json", auth(s.handleStatsJSON))
	s.mux.Handle("GET /sessions/{id}/targets/{tid}/screenshot.jpg", auth(s.handleScreenshot))
	s.mux.Handle("GET /sessions/{id}/chat.json", auth(s.handleChatJSON))
	s.mux.Handle("GET /sessions/{id}/chat.txt", auth(s.handleChatExport))
	s.mux.Handle("GET /events", auth(s.handleEvents))
	s.mux.Handle("POST /keys", auth(s.handleCreateKey))
	s.mux.Handle("POST /keys/remove", auth(s.handleRemoveKey))
	s.mux.Handle("GET /recordings", auth(s.handleRecordings))
	s.mux.Handle("GET /recordings/files/", auth(http.StripPrefix("/recordings/files/", http.FileServer(http.Dir(s.cfg.MediaMTX.RecordingsDir))).ServeHTTP))
	s.mux.Handle("GET /admin", admin(s.handleAdmin))
	s.mux.Handle("POST /admin/whitelist", admin(s.handleWhitelistAdd))
	s.mux.Handle("POST /admin/whitelist/remove", admin(s.handleWhitelistRemove))
	s.mux.Handle("POST /admin/identities", admin(s.handleIdentityAdd))
	s.mux.Handle("POST /admin/identities/remove", admin(s.handleIdentityRemove))
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["User"] = userFrom(r.Context())
	data["Page"] = strings.TrimSuffix(name, ".html")
	data["PublicURL"] = s.cfg.PublicURL
	data["V"] = assetVer
	t, ok := s.tmpl[name]
	if !ok {
		http.Error(w, "no template "+name, 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := t.ExecuteTemplate(w, "layout.html", data); err != nil {
		s.log.Error("render", "template", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error, back string) {
	s.log.Warn("web", "path", r.URL.Path, "err", err)
	http.Redirect(w, r, back+"?error="+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	running, err := s.mgr.ListViews(r.Context(), true, 50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	past, _ := s.mgr.ListViews(r.Context(), false, 20)
	var recent []session.SessionView
	for _, v := range past {
		if v.Status != "running" {
			recent = append(recent, v)
		}
	}
	keys, _ := s.store.ListStreamKeys(r.Context(), u.ID)
	type keyRow struct {
		store.StreamKey
		WHIP, RTMP string
	}
	var krows []keyRow
	for _, k := range keys {
		krows = append(krows, keyRow{k, s.media.PublicWHIPURL(k.Key), s.media.PublicRTMPURL()})
	}
	s.render(w, r, "dashboard.html", map[string]any{
		"Running": running, "Recent": recent, "Keys": krows, "Error": r.URL.Query().Get("error"),
	})
}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	chans, err := s.slack.ListChannels(r.Context())
	if err != nil {
		s.log.Warn("list channels", "err", err)
	}
	sort.Slice(chans, func(i, j int) bool { return chans[i].Name < chans[j].Name })
	keys, _ := s.store.ListStreamKeys(r.Context(), u.ID)
	s.render(w, r, "new.html", map[string]any{"Channels": chans, "Keys": keys, "Quality": s.cfg.Quality, "Error": r.URL.Query().Get("error")})
}

func applyQualityForm(q config.Quality, r *http.Request) (config.Quality, bool) {
	changed := false
	if v := r.FormValue("q_profile"); v == "music-stereo" || v == "music-mono" || v == "speech" {
		q.Audio.Profile = v
		changed = true
	}
	for _, f := range []struct {
		name string
		dst  *int
	}{
		{"q_audio_kbps", &q.Audio.MaxBitrateKbps}, {"q_video_kbps", &q.Video.MaxBitrateKbps},
		{"q_fps", &q.Video.FPS}, {"q_width", &q.Video.Width}, {"q_height", &q.Video.Height},
	} {
		if n, err := strconv.Atoi(r.FormValue(f.name)); err == nil && n > 0 {
			*f.dst = n
			changed = true
		}
	}
	return q, changed
}

func (s *Server) parseQuality(r *http.Request) *config.Quality {
	q, changed := applyQualityForm(s.cfg.Quality, r)
	if !changed {
		return nil
	}
	return &q
}

func (s *Server) handleStatsJSON(w http.ResponseWriter, r *http.Request) {
	v, err := s.mgr.View(r.Context(), r.PathValue("id"))
	if err != nil || v == nil {
		http.NotFound(w, r)
		return
	}
	type row struct {
		TargetID      int64  `json:"targetId"`
		Status        string `json:"status"`
		Error         string `json:"error"`
		Res           string `json:"res"`
		SrcFPS        int    `json:"srcFps"`
		SrcKbps       int    `json:"srcKbps"`
		PaintFPS      int    `json:"paintFps"`
		TargetFPS     int    `json:"targetFps"`
		FramesDropped int    `json:"framesDropped"`
		DropRate      int    `json:"dropRate"`
		Standby       bool   `json:"standby"`
		Participants  int    `json:"participants"`
	}
	var out []row
	for _, t := range v.Targets {
		rw := row{TargetID: t.ID, Status: t.Status, Participants: -1}
		if t.Snap != nil {
			rw.Status = string(t.Snap.Status)
			rw.Error = t.Snap.Error
			rw.Participants = t.Snap.Participants
			if st := t.Snap.Stats; st != nil {
				rw.Res = st.SourceRes
				rw.SrcFPS = st.SourceFPS
				rw.SrcKbps = st.SourceKbps
				rw.PaintFPS = st.PaintFPS
				rw.TargetFPS = st.TargetFPS
				rw.FramesDropped = st.FramesDropped
				rw.DropRate = st.DropRate
				rw.Standby = st.Standby
			}
		}
		out = append(out, rw)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) handleRecordToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.SetRecord(r.Context(), id, r.FormValue("record") == "on"); err != nil {
		s.fail(w, r, err, "/sessions/"+id)
		return
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleQuality(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, ok := s.mgr.Quality(id)
	if !ok {
		s.fail(w, r, fmt.Errorf("session not running"), "/sessions/"+id)
		return
	}
	q, _ := applyQualityForm(cur, r)
	if err := s.mgr.SetQuality(id, q); err != nil {
		s.fail(w, r, err, "/sessions/"+id)
		return
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleStartSession(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err, "/sessions/new")
		return
	}
	refs := append([]string{}, r.Form["targets"]...)
	for _, extra := range strings.Fields(strings.ReplaceAll(r.FormValue("extra"), ",", " ")) {
		refs = append(refs, extra)
	}
	targets, terr := s.slack.PrepareTargets(r.Context(), refs)
	if terr != nil && len(targets) == 0 {
		s.fail(w, r, terr, "/sessions/new")
		return
	}
	req := session.StartRequest{CreatedBy: u.ID, SourceType: r.FormValue("source_type"), Presentation: r.FormValue("presentation"), Targets: targets, Quality: s.parseQuality(r), Record: r.FormValue("record") == "on"}
	if req.Presentation == "" {
		req.Presentation = huddle.PresentationScreen
	}
	switch req.SourceType {
	case session.SourceMirror:
		c, err := s.slack.ResolveChannel(r.Context(), r.FormValue("mirror_channel"))
		if err != nil {
			s.fail(w, r, err, "/sessions/new")
			return
		}
		s.slack.JoinChannel(r.Context(), c.ID)
		req.SourceRef = c.ID
	default:
		req.SourceType = session.SourcePush
		req.SourceRef = r.FormValue("stream_key")
	}
	sess, err := s.mgr.Start(r.Context(), req)
	if err != nil {
		s.fail(w, r, err, "/sessions/new")
		return
	}
	s.slack.AnnounceStart(r.Context(), sess, targets)
	http.Redirect(w, r, "/sessions/"+sess.ID, http.StatusSeeOther)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	v, err := s.mgr.View(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if v == nil {
		http.NotFound(w, r)
		return
	}
	chat, _ := s.store.ListChat(r.Context(), v.ID, 0, 500)
	var obsWHIP, obsRTMP string
	if v.SourceType == session.SourcePush {
		obsWHIP, obsRTMP = s.media.PublicWHIPURL(v.SourceRef), s.media.PublicRTMPURL()
	}
	q, ok := s.mgr.Quality(v.ID)
	if !ok {
		q = s.cfg.Quality
	}
	rec, _ := s.mgr.Recording(v.ID)
	spath := media.PushPath(v.SourceRef)
	if v.SourceType == session.SourceMirror {
		spath = media.MirrorPath(v.ID)
	}
	s.render(w, r, "session.html", map[string]any{
		"S": v, "Chat": chat, "OBSWHIP": obsWHIP, "OBSRTMP": obsRTMP, "Error": r.URL.Query().Get("error"),
		"Presentations": []string{huddle.PresentationScreen, huddle.PresentationCamera, huddle.PresentationBoth},
		"Quality":       q, "Record": rec, "PreviewURL": s.media.PublicWHEPURL(spath),
	})
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.Stop(r.Context(), id); err != nil {
		s.fail(w, r, err, "/sessions/"+id)
		return
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleAddTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := s.slack.ResolveChannel(r.Context(), r.FormValue("channel"))
	if err != nil {
		s.fail(w, r, err, "/sessions/"+id)
		return
	}
	s.slack.JoinChannel(r.Context(), c.ID)
	if err := s.mgr.AddTarget(r.Context(), id, c); err != nil {
		s.fail(w, r, err, "/sessions/"+id)
		return
	}
	s.slack.Announce(r.Context(), c.ID, fmt.Sprintf(":satellite_antenna: huddlecast is streaming into this channel's huddle (session `%s`).", id))
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleRemoveTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tid, _ := strconv.ParseInt(r.PathValue("tid"), 10, 64)
	if err := s.mgr.RemoveTarget(r.Context(), id, tid); err != nil {
		s.fail(w, r, err, "/sessions/"+id)
		return
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handlePresentation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.SetPresentation(r.Context(), id, r.FormValue("presentation")); err != nil {
		s.fail(w, r, err, "/sessions/"+id)
		return
	}
	http.Redirect(w, r, "/sessions/"+id, http.StatusSeeOther)
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	tid, _ := strconv.ParseInt(r.PathValue("tid"), 10, 64)
	img := s.mgr.Screenshot(r.PathValue("id"), tid)
	if img == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(img)
}

func (s *Server) handleChatJSON(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	msgs, err := s.store.ListChat(r.Context(), r.PathValue("id"), after, 500)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msgs)
}

func (s *Server) handleChatExport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="huddlecast-chat-%s.txt"`, id))
	var after int64
	for {
		msgs, err := s.store.ListChat(r.Context(), id, after, 500)
		if err != nil || len(msgs) == 0 {
			return
		}
		for _, m := range msgs {
			fmt.Fprintf(w, "[%s] #%s <%s> %s\n", m.ReceivedAt, m.ChannelName, m.UserName, m.Text)
			after = m.ID
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	filter := r.URL.Query().Get("session")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsub := s.mgr.Events().Subscribe()
	defer unsub()
	fmt.Fprint(w, ": hello\n\n")
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case e := <-ch:
			if filter != "" && e.SessionID != filter {
				continue
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, b)
			fl.Flush()
		}
	}
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	k := store.StreamKey{Key: slackapp.NewStreamKey(), OwnerID: u.ID, Label: r.FormValue("label")}
	if err := s.store.CreateStreamKey(r.Context(), k); err != nil {
		s.fail(w, r, err, "/")
		return
	}
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

func (s *Server) handleRemoveKey(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	k, err := s.store.GetStreamKey(r.Context(), r.FormValue("key"))
	if err != nil || k == nil || (k.OwnerID != u.ID && !u.Admin) {
		http.Error(w, "no such key", http.StatusForbidden)
		return
	}
	_ = s.store.DeleteStreamKey(r.Context(), k.Key)
	http.Redirect(w, r, r.Referer(), http.StatusSeeOther)
}

func (s *Server) handleRecordings(w http.ResponseWriter, r *http.Request) {
	root := s.cfg.MediaMTX.RecordingsDir
	recs, _ := s.store.ListRecordings(r.Context(), 500)
	type recRow struct {
		Name, StreamKey, Storage, CreatedAt, URL string
		Size                                     int64
	}
	var rows []recRow
	for _, rec := range recs {
		rows = append(rows, recRow{
			Name: rec.Name, StreamKey: rec.StreamKey, Storage: rec.Storage,
			CreatedAt: rec.CreatedAt, Size: rec.Size, URL: s.rec.URL(rec),
		})
	}
	_, statErr := os.Stat(root)
	s.render(w, r, "recordings.html", map[string]any{
		"Recordings": rows, "Dir": root, "Missing": statErr != nil, "R2": s.cfg.Recording.R2.Enabled(),
	})
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	wl, _ := s.store.ListWhitelist(r.Context())
	accts, _ := s.store.ListAccountStates(r.Context())
	known := map[string]store.AccountState{}
	for _, a := range accts {
		known[a.Name] = a
	}
	type acctRow struct {
		Name, Status, LastError, UpdatedAt string
	}
	var rows []acctRow
	for _, a := range s.cfg.Accounts {
		st := known[a.Name]
		if st.Status == "" {
			st.Status = "unused"
		}
		rows = append(rows, acctRow{a.Name, st.Status, st.LastError, st.UpdatedAt})
	}
	idsRaw, _ := s.store.ListIdentities(r.Context())
	type idRow struct {
		Name, Mode, Status, LastError, UpdatedAt, CookieHint string
	}
	var ids []idRow
	for _, id := range idsRaw {
		hint := "set"
		if len(id.CookieD) >= 6 {
			hint = "…" + id.CookieD[len(id.CookieD)-4:]
		}
		ids = append(ids, idRow{id.Name, id.Mode, id.Status, id.LastError, id.UpdatedAt, hint})
	}
	keys, _ := s.store.ListStreamKeys(r.Context(), "")
	var mtxErr string
	if err := s.media.Healthy(r.Context()); err != nil {
		mtxErr = err.Error()
	}
	s.render(w, r, "admin.html", map[string]any{
		"Whitelist": wl, "Accounts": rows, "Identities": ids, "Keys": keys, "MediaMTXError": mtxErr, "Error": r.URL.Query().Get("error"),
	})
}

func (s *Server) handleWhitelistAdd(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	id := strings.TrimSpace(r.FormValue("user_id"))
	if m := strings.TrimPrefix(strings.TrimSuffix(id, ">"), "<@"); m != id {
		id = strings.SplitN(m, "|", 2)[0]
	}
	if id == "" {
		s.fail(w, r, fmt.Errorf("user id required"), "/admin")
		return
	}
	name := r.FormValue("name")
	if name == "" && s.slack != nil {
		name = s.slack.UserName(r.Context(), id)
	}
	err := s.store.UpsertWhitelist(r.Context(), store.WhitelistEntry{UserID: id, Name: name, Admin: r.FormValue("admin") == "on", AddedBy: u.ID})
	if err != nil {
		s.fail(w, r, err, "/admin")
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleWhitelistRemove(w http.ResponseWriter, r *http.Request) {
	_ = s.store.RemoveWhitelist(r.Context(), r.FormValue("user_id"))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleIdentityAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	cookie := strings.TrimSpace(r.FormValue("cookie_d"))
	mode := r.FormValue("mode")
	if name == "" || cookie == "" {
		s.fail(w, r, fmt.Errorf("identity name and d cookie are required"), "/admin")
		return
	}
	if err := s.store.UpsertIdentity(r.Context(), store.Identity{Name: name, CookieD: cookie, Mode: mode}); err != nil {
		s.fail(w, r, err, "/admin")
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleIdentityRemove(w http.ResponseWriter, r *http.Request) {
	_ = s.store.DeleteIdentity(r.Context(), r.FormValue("name"))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{Addr: s.cfg.Listen, Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	s.log.Info("web ui listening", "addr", s.cfg.Listen, "public", s.cfg.PublicURL)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
