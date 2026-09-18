package slackapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/LeafdTK/huddlecast/internal/config"
	"github.com/LeafdTK/huddlecast/internal/session"
	"github.com/LeafdTK/huddlecast/internal/slackrooms"
	"github.com/LeafdTK/huddlecast/internal/store"
)

type App struct {
	cfg   *config.Config
	store *store.Store
	mgr   *session.Manager
	api   *slack.Client
	sm    *socketmode.Client
	rooms *slackrooms.Client
	log   *slog.Logger

	mu       sync.Mutex
	users    map[string]string
	channels map[string]string
	roots    map[string]bool
	ownIDs   map[string]bool
}

func New(cfg *config.Config, st *store.Store, mgr *session.Manager, log *slog.Logger) *App {
	api := slack.New(cfg.Slack.BotToken, slack.OptionAppLevelToken(cfg.Slack.AppToken))
	a := &App{
		cfg: cfg, store: st, mgr: mgr, api: api, log: log,
		users: map[string]string{}, channels: map[string]string{}, roots: map[string]bool{}, ownIDs: map[string]bool{},
	}
	for _, acc := range cfg.Accounts {
		if acc.UserID != "" {
			a.ownIDs[acc.UserID] = true
		}
	}
	if cfg.Slack.Host != "" && len(cfg.Accounts) > 0 && cfg.Accounts[0].CookieD != "" {
		a.rooms = slackrooms.New(cfg.Slack.Host, cfg.Accounts[0].CookieD)
	}
	a.sm = socketmode.New(api)
	return a
}

func (a *App) API() *slack.Client { return a.api }

func (a *App) SeedWhitelist(ctx context.Context) error {
	for _, id := range a.cfg.Slack.Admins {
		name := ""
		if a.cfg.Slack.SocketMode() {
			name = a.UserName(ctx, id)
		}
		if err := a.store.UpsertWhitelist(ctx, store.WhitelistEntry{UserID: id, Name: name, Admin: true, AddedBy: "config"}); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) Run(ctx context.Context) error {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-a.sm.Events:
				if !ok {
					return
				}
				a.handle(ctx, evt)
			}
		}
	}()
	return a.sm.RunContext(ctx)
}

func (a *App) handle(ctx context.Context, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeConnected:
		a.log.Info("slack socket mode connected")
	case socketmode.EventTypeSlashCommand:
		cmd, ok := evt.Data.(slack.SlashCommand)
		if !ok || evt.Request == nil {
			return
		}
		text := a.command(ctx, cmd)
		_ = a.sm.Ack(*evt.Request, map[string]any{"response_type": "ephemeral", "text": text})
	case socketmode.EventTypeEventsAPI:
		if evt.Request != nil {
			_ = a.sm.Ack(*evt.Request)
		}
		api, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		if msg, ok := api.InnerEvent.Data.(*slackevents.MessageEvent); ok {
			a.message(ctx, msg)
		}
	}
}

func (a *App) message(ctx context.Context, m *slackevents.MessageEvent) {
	if m.SubType == "huddle_thread" {
		a.mu.Lock()
		a.roots[m.Channel+"|"+m.TimeStamp] = true
		a.mu.Unlock()
		if sid, ok := a.mgr.SessionForChannel(m.Channel); ok {
			_ = a.store.SetTargetHuddleRoot(ctx, sid, m.Channel, m.TimeStamp)
		}
		return
	}
	if m.ThreadTimeStamp == "" || m.BotID != "" || m.User == "" || a.ownIDs[m.User] {
		return
	}
	if m.SubType != "" && m.SubType != "thread_broadcast" && m.SubType != "file_share" {
		return
	}
	sid, ok := a.mgr.SessionForChannel(m.Channel)
	if !ok || !a.isHuddleRoot(ctx, m.Channel, m.ThreadTimeStamp) {
		return
	}
	msg := store.ChatMessage{
		SessionID: sid, ChannelID: m.Channel, ChannelName: a.ChannelName(ctx, m.Channel),
		UserID: m.User, UserName: a.UserName(ctx, m.User), Text: m.Text, TS: m.TimeStamp,
	}
	id, err := a.store.AddChat(ctx, msg)
	if err != nil {
		a.log.Error("store chat", "err", err)
		return
	}
	msg.ID = id
	a.mgr.Events().Publish(session.Event{Type: "chat", SessionID: sid, Data: msg})
}

func (a *App) isHuddleRoot(ctx context.Context, channel, ts string) bool {
	key := channel + "|" + ts
	a.mu.Lock()
	v, cached := a.roots[key]
	a.mu.Unlock()
	if cached {
		return v
	}
	res, err := a.api.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
		ChannelID: channel, Latest: ts, Inclusive: true, Limit: 1,
	})
	is := err == nil && len(res.Messages) == 1 && res.Messages[0].SubType == "huddle_thread"
	if err == nil {
		a.mu.Lock()
		a.roots[key] = is
		a.mu.Unlock()
	}
	return is
}

func (a *App) UserName(ctx context.Context, id string) string {
	a.mu.Lock()
	n, ok := a.users[id]
	a.mu.Unlock()
	if ok {
		return n
	}
	u, err := a.api.GetUserInfoContext(ctx, id)
	if err != nil {
		return id
	}
	n = u.Profile.DisplayName
	if n == "" {
		n = u.RealName
	}
	if n == "" {
		n = u.Name
	}
	a.mu.Lock()
	a.users[id] = n
	a.mu.Unlock()
	return n
}

func (a *App) ChannelName(ctx context.Context, id string) string {
	a.mu.Lock()
	n, ok := a.channels[id]
	a.mu.Unlock()
	if ok {
		return n
	}
	if !a.cfg.Slack.SocketMode() && a.rooms != nil {
		if name, err := a.rooms.ConversationName(ctx, id); err == nil && name != "" {
			a.mu.Lock()
			a.channels[id] = name
			a.mu.Unlock()
			return name
		}
		return id
	}
	c, err := a.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: id})
	if err != nil {
		return id
	}
	a.mu.Lock()
	a.channels[id] = c.Name
	a.mu.Unlock()
	return c.Name
}

func (a *App) ListChannels(ctx context.Context) ([]session.Channel, error) {
	if !a.cfg.Slack.SocketMode() && a.rooms != nil {
		convs, err := a.rooms.ListConversations(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]session.Channel, 0, len(convs))
		for _, c := range convs {
			out = append(out, session.Channel{ID: c.ID, Name: c.Name})
			a.mu.Lock()
			a.channels[c.ID] = c.Name
			a.mu.Unlock()
		}
		return out, nil
	}
	var out []session.Channel
	cursor := ""
	for {
		chans, next, err := a.api.GetConversationsContext(ctx, &slack.GetConversationsParameters{
			Types: []string{"public_channel", "private_channel"}, ExcludeArchived: true, Limit: 1000, Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		for _, c := range chans {
			out = append(out, session.Channel{ID: c.ID, Name: c.Name})
			a.mu.Lock()
			a.channels[c.ID] = c.Name
			a.mu.Unlock()
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

var (
	mentionRe = regexp.MustCompile(`^<#([A-Z0-9]+)(?:\|([^>]*))?>$`)
	idRe      = regexp.MustCompile(`^[CG][A-Z0-9]{6,}$`)
)

func (a *App) ResolveChannel(ctx context.Context, ref string) (session.Channel, error) {
	ref = strings.TrimSpace(ref)
	if m := mentionRe.FindStringSubmatch(ref); m != nil {
		name := m[2]
		if name == "" {
			name = a.ChannelName(ctx, m[1])
		}
		return session.Channel{ID: m[1], Name: name}, nil
	}
	if idRe.MatchString(ref) {
		return session.Channel{ID: ref, Name: a.ChannelName(ctx, ref)}, nil
	}
	name := strings.TrimPrefix(ref, "#")
	chans, err := a.ListChannels(ctx)
	if err != nil {
		return session.Channel{}, err
	}
	for _, c := range chans {
		if strings.EqualFold(c.Name, name) {
			return c, nil
		}
	}
	return session.Channel{}, fmt.Errorf("unknown channel %q", ref)
}

func (a *App) JoinChannel(ctx context.Context, id string) {
	if !a.cfg.Slack.SocketMode() {
		return
	}
	if _, _, _, err := a.api.JoinConversationContext(ctx, id); err != nil && !strings.Contains(err.Error(), "already_in_channel") {
		a.log.Warn("join channel", "channel", id, "err", err)
	}
}

func (a *App) Announce(ctx context.Context, channelID, text string) {
	if !a.cfg.Slack.SocketMode() {
		return
	}
	if _, _, err := a.api.PostMessageContext(ctx, channelID, slack.MsgOptionText(text, false)); err != nil {
		a.log.Warn("announce", "channel", channelID, "err", err)
	}
}

func (a *App) AnnounceStart(ctx context.Context, sess *store.Session, targets []session.Channel) {
	for _, t := range targets {
		a.JoinChannel(ctx, t.ID)
		a.Announce(ctx, t.ID, fmt.Sprintf(":satellite_antenna: huddlecast is streaming into this channel's huddle (session `%s`). Messages in the huddle thread show up in the unified chat feed.", sess.ID))
	}
}

func (a *App) PrepareTargets(ctx context.Context, refs []string) ([]session.Channel, error) {
	var out []session.Channel
	var errs []error
	for _, r := range refs {
		c, err := a.ResolveChannel(ctx, r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		a.JoinChannel(ctx, c.ID)
		out = append(out, c)
	}
	return out, errors.Join(errs...)
}
