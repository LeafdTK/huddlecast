package slackapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/slack-go/slack"

	"github.com/LeafdTK/huddlecast/internal/huddle"
	"github.com/LeafdTK/huddlecast/internal/session"
	"github.com/LeafdTK/huddlecast/internal/store"
)

func NewStreamKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) help() string {
	c := a.cfg.Slack.SlashCommand
	return strings.Join([]string{
		"*huddlecast* — stream one source into many huddles",
		fmt.Sprintf("`%s key [label]` — create a stream key and get OBS settings", c),
		fmt.Sprintf("`%s keys` — list your stream keys", c),
		fmt.Sprintf("`%s start #a #b … [--key KEY] [--mirror #source] [--camera|--both]` — start streaming", c),
		fmt.Sprintf("`%s add #c` — add a channel to your running session", c),
		fmt.Sprintf("`%s remove #c` — drop a channel", c),
		fmt.Sprintf("`%s present screen|camera|both` — change how the stream appears", c),
		fmt.Sprintf("`%s stop [SESSION]` — stop streaming", c),
		fmt.Sprintf("`%s status` — what's running", c),
		fmt.Sprintf("web ui: %s", a.cfg.PublicURL),
	}, "\n")
}

func (a *App) command(ctx context.Context, cmd slack.SlashCommand) string {
	ok, err := a.store.IsWhitelisted(ctx, cmd.UserID)
	if err != nil {
		return "error: " + err.Error()
	}
	if !ok {
		a.log.Warn("slash command from non-whitelisted user", "user", cmd.UserID, "text", cmd.Text)
		return "you're not on the huddlecast whitelist. ask an admin to add you."
	}
	args := strings.Fields(cmd.Text)
	if len(args) == 0 {
		return a.help()
	}
	switch args[0] {
	case "help":
		return a.help()
	case "key":
		return a.cmdKey(ctx, cmd.UserID, strings.Join(args[1:], " "))
	case "keys":
		return a.cmdKeys(ctx, cmd.UserID)
	case "start":
		return a.cmdStart(ctx, cmd.UserID, args[1:])
	case "add", "remove":
		return a.cmdAddRemove(ctx, cmd.UserID, args[0], args[1:])
	case "present":
		return a.cmdPresent(ctx, cmd.UserID, args[1:])
	case "stop":
		return a.cmdStop(ctx, cmd.UserID, args[1:])
	case "status":
		return a.cmdStatus(ctx)
	}
	return "unknown subcommand. " + a.help()
}

func (a *App) obsHelp(key string) string {
	m := a.mgr.Media()
	return fmt.Sprintf("OBS → Settings → Stream:\n• WHIP (preferred): server `%s`, bearer token `%s`\n• RTMP fallback: server `%s`, stream key `%s`",
		m.PublicWHIPURL(key), key, m.PublicRTMPURL(), key)
}

func (a *App) cmdKey(ctx context.Context, user, label string) string {
	k := store.StreamKey{Key: NewStreamKey(), OwnerID: user, Label: label}
	if err := a.store.CreateStreamKey(ctx, k); err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("created stream key `%s`\n%s", k.Key, a.obsHelp(k.Key))
}

func (a *App) cmdKeys(ctx context.Context, user string) string {
	keys, err := a.store.ListStreamKeys(ctx, user)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(keys) == 0 {
		return "no stream keys yet. run `" + a.cfg.Slack.SlashCommand + " key`."
	}
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "• `%s` %s\n", k.Key, k.Label)
	}
	return b.String()
}

func (a *App) cmdStart(ctx context.Context, user string, args []string) string {
	req := session.StartRequest{CreatedBy: user, SourceType: session.SourcePush, Presentation: huddle.PresentationScreen}
	var refs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--key":
			if i+1 < len(args) {
				req.SourceRef = args[i+1]
				i++
			}
		case "--mirror":
			if i+1 < len(args) {
				c, err := a.ResolveChannel(ctx, args[i+1])
				if err != nil {
					return err.Error()
				}
				req.SourceType, req.SourceRef = session.SourceMirror, c.ID
				a.JoinChannel(ctx, c.ID)
				i++
			}
		case "--camera":
			req.Presentation = huddle.PresentationCamera
		case "--both":
			req.Presentation = huddle.PresentationBoth
		case "--screen":
			req.Presentation = huddle.PresentationScreen
		default:
			refs = append(refs, args[i])
		}
	}
	if req.SourceType == session.SourcePush && req.SourceRef == "" {
		keys, err := a.store.ListStreamKeys(ctx, user)
		if err != nil {
			return "error: " + err.Error()
		}
		switch len(keys) {
		case 0:
			return "you have no stream key. run `" + a.cfg.Slack.SlashCommand + " key` first, or use `--mirror #channel`."
		case 1:
			req.SourceRef = keys[0].Key
		default:
			return "you have several stream keys; pass `--key KEY`."
		}
	}
	targets, err := a.PrepareTargets(ctx, refs)
	if err != nil && len(targets) == 0 {
		return err.Error()
	}
	req.Targets = targets
	sess, err := a.mgr.Start(ctx, req)
	if err != nil {
		return "error: " + err.Error()
	}
	a.AnnounceStart(ctx, sess, targets)
	names := make([]string, len(targets))
	for i, t := range targets {
		names[i] = "#" + t.Name
	}
	out := fmt.Sprintf("started session `%s` → %s (%s)\n%s/sessions/%s", sess.ID, strings.Join(names, " "), req.Presentation, a.cfg.PublicURL, sess.ID)
	if req.SourceType == session.SourcePush {
		out += "\n" + a.obsHelp(req.SourceRef)
	}
	if err != nil {
		out += "\nskipped: " + err.Error()
	}
	return out
}

func (a *App) userSession(ctx context.Context, user string, explicit string) (string, string) {
	if explicit != "" {
		return explicit, ""
	}
	var mine []string
	for _, id := range a.mgr.Running() {
		s, err := a.store.GetSession(ctx, id)
		if err == nil && s != nil && s.CreatedBy == user {
			mine = append(mine, id)
		}
	}
	switch len(mine) {
	case 0:
		return "", "you have no running session."
	case 1:
		return mine[0], ""
	}
	return "", "you have several running sessions (" + strings.Join(mine, ", ") + "); pass the session id."
}

func (a *App) cmdAddRemove(ctx context.Context, user, verb string, args []string) string {
	var refs []string
	explicit := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--session" && i+1 < len(args) {
			explicit = args[i+1]
			i++
			continue
		}
		refs = append(refs, args[i])
	}
	sid, msg := a.userSession(ctx, user, explicit)
	if msg != "" {
		return msg
	}
	var out []string
	for _, r := range refs {
		c, err := a.ResolveChannel(ctx, r)
		if err != nil {
			out = append(out, err.Error())
			continue
		}
		if verb == "add" {
			a.JoinChannel(ctx, c.ID)
			err = a.mgr.AddTarget(ctx, sid, c)
			if err == nil {
				a.Announce(ctx, c.ID, fmt.Sprintf(":satellite_antenna: huddlecast is streaming into this channel's huddle (session `%s`).", sid))
			}
		} else {
			err = a.mgr.RemoveTargetByChannel(ctx, sid, c.ID)
		}
		if err != nil {
			out = append(out, "#"+c.Name+": "+err.Error())
		} else {
			out = append(out, "#"+c.Name+": ok")
		}
	}
	if len(out) == 0 {
		return "which channel?"
	}
	return strings.Join(out, "\n")
}

func (a *App) cmdPresent(ctx context.Context, user string, args []string) string {
	if len(args) == 0 {
		return "usage: present screen|camera|both [--session ID]"
	}
	explicit := ""
	if len(args) >= 3 && args[1] == "--session" {
		explicit = args[2]
	}
	sid, msg := a.userSession(ctx, user, explicit)
	if msg != "" {
		return msg
	}
	if err := a.mgr.SetPresentation(ctx, sid, args[0]); err != nil {
		return "error: " + err.Error()
	}
	return "presentation → " + args[0]
}

func (a *App) cmdStop(ctx context.Context, user string, args []string) string {
	explicit := ""
	if len(args) > 0 {
		explicit = args[0]
	}
	sid, msg := a.userSession(ctx, user, explicit)
	if msg != "" {
		return msg
	}
	if err := a.mgr.Stop(ctx, sid); err != nil {
		return "error: " + err.Error()
	}
	return "stopped `" + sid + "`"
}

func (a *App) cmdStatus(ctx context.Context) string {
	views, err := a.mgr.ListViews(ctx, true, 20)
	if err != nil {
		return "error: " + err.Error()
	}
	if len(views) == 0 {
		return "nothing running."
	}
	var b strings.Builder
	for _, v := range views {
		live := "source offline"
		if v.SourceLive {
			live = "source live"
		}
		fmt.Fprintf(&b, "*%s* by <@%s> — %s/%s — %s — %s\n", v.ID, v.CreatedBy, v.SourceType, v.SourceRef, v.Presentation, live)
		for _, t := range v.Targets {
			st := t.Status
			if t.Snap != nil {
				st = string(t.Snap.Status)
				if t.Snap.Participants >= 0 {
					st += fmt.Sprintf(" (%d in huddle)", t.Snap.Participants)
				}
			}
			fmt.Fprintf(&b, "  • #%s [%s] %s %s\n", t.ChannelName, t.Account, st, t.LastError)
		}
	}
	return b.String()
}
