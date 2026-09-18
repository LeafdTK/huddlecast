package huddle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"

	"github.com/LeafdTK/huddlecast/internal/browser"
)

type Driver struct {
	B   *browser.Browser
	Log *slog.Logger
}

var ErrAccessDenied = errors.New("huddle access denied: is the account a member of the channel?")

var ErrNoJoinButton = errors.New("could not find a join button (run `huddlecast probe` and update selectors.go)")

func (d *Driver) Find(sel Selector) *rod.Element { return d.find(sel) }

func (d *Driver) find(sel Selector) *rod.Element {
	page := d.B.Page()
	for _, css := range sel.CSS {
		el, err := page.Timeout(500 * time.Millisecond).Element(css)
		if err == nil && el != nil {
			el = el.Context(page.GetContext()).Timeout(10 * time.Second)
			if vis, _ := el.Visible(); vis {
				return el
			}
		}
	}
	if sel.TextRegexp != "" {
		re := regexp.MustCompile(sel.TextRegexp)
		els, err := page.Timeout(2 * time.Second).Elements("button, a[role=button], [role=button]")
		if err == nil {
			for _, el := range els {
				el = el.Context(page.GetContext()).Timeout(10 * time.Second)
				txt, _ := el.Text()
				if re.MatchString(strings.TrimSpace(txt)) {
					if vis, _ := el.Visible(); vis {
						return el
					}
				}
			}
		}
	}
	return nil
}

func (d *Driver) waitFor(ctx context.Context, sel Selector, timeout time.Duration) *rod.Element {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if el := d.find(sel); el != nil {
			return el
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(750 * time.Millisecond):
		}
	}
	return nil
}

func (d *Driver) click(el *rod.Element) error {
	if err := el.ScrollIntoView(); err != nil {
		return err
	}
	return el.Click(proto.InputMouseButtonLeft, 1)
}

func (d *Driver) InHuddle() bool {
	if d.find(Join) != nil {
		return false
	}
	return d.B.HasHuddleWindow() || d.find(Leave) != nil
}

func (d *Driver) Join(ctx context.Context, teamID, channelID string, timeout time.Duration) error {
	if d.InHuddle() {
		return nil
	}
	if d.find(AccessDenied) != nil {
		return ErrAccessDenied
	}

	if el := d.waitFor(ctx, Join, timeout/2); el != nil {
		d.Log.Info("huddle: clicking join")
		if err := d.click(el); err != nil {
			return fmt.Errorf("click join: %w", err)
		}
		if d.waitInHuddle(ctx, timeout/2) {
			return nil
		}
		if d.find(AccessDenied) != nil {
			return ErrAccessDenied
		}
	}

	d.Log.Info("huddle: deep link had no join button, trying client view toggle")
	if err := d.B.Page().Navigate(ClientURL(teamID, channelID)); err != nil {
		return err
	}
	_ = d.B.Page().Timeout(30 * time.Second).WaitLoad()
	if err := d.B.CheckLoggedIn(); err != nil {
		return err
	}
	if el := d.waitFor(ctx, Toggle, timeout/2); el != nil {
		triggers := []struct {
			name string
			fn   func() error
		}{
			{"cdp click", func() error { return d.click(el) }},
			{"js click", func() error {
				el := d.find(Toggle)
				if el == nil {
					return errors.New("toggle vanished")
				}
				_, err := el.Eval(`() => this.click()`)
				return err
			}},
			{"keyboard shortcut", func() error {
				return d.B.Page().KeyActions().Press(input.MetaLeft).Press(input.AltLeft).Press(input.ShiftLeft).Type(input.KeyH).Do()
			}},
		}
		for _, t := range triggers {
			d.Log.Info("huddle: triggering header huddle button", "via", t.name)
			if err := t.fn(); err != nil {
				d.Log.Warn("huddle: trigger failed", "via", t.name, "err", err)
				continue
			}
			if d.waitInHuddle(ctx, 12*time.Second) {
				return nil
			}
			if el := d.find(Join); el != nil {
				d.Log.Info("huddle: clicking join on confirmation")
				_ = d.click(el)
				if d.waitInHuddle(ctx, 12*time.Second) {
					return nil
				}
			}
		}
	}
	d.Log.Warn("huddle: no huddle ui found", "pages", d.B.PageURLs())
	return ErrNoJoinButton
}

func (d *Driver) Leave() {
	if el := d.find(Leave); el != nil {
		_ = d.click(el)
		time.Sleep(1500 * time.Millisecond)
	}
}

func (d *Driver) EnsureUnmuted() {
	if el := d.find(Unmute); el != nil {
		d.Log.Info("huddle: unmuting")
		_ = d.click(el)
	}
}

func (d *Driver) EnsureMuted() {
	if el := d.find(Mute); el != nil {
		d.Log.Info("huddle: muting")
		_ = d.click(el)
	}
}

func (d *Driver) IsSharing() bool { return d.find(StopShare) != nil }

func (d *Driver) StartShare(ctx context.Context) error {
	if d.IsSharing() {
		return nil
	}
	el := d.waitFor(ctx, ShareScreen, 15*time.Second)
	if el == nil {
		return errors.New("share screen button not found")
	}
	d.Log.Info("huddle: starting screen share")
	if err := d.click(el); err != nil {
		return err
	}

	if !d.waitForBool(ctx, d.IsSharing, 10*time.Second) {
		if el2 := d.find(ShareScreen); el2 != nil {
			_ = d.click(el2)
		}
		if !d.waitForBool(ctx, d.IsSharing, 10*time.Second) {
			return errors.New("screen share did not start (no stop-sharing control appeared)")
		}
	}
	return nil
}

func (d *Driver) StopShare() {
	if el := d.find(StopShare); el != nil {
		_ = d.click(el)
	}
}

func (d *Driver) IsCameraOn() bool { return d.find(CameraOff) != nil }

func (d *Driver) SetCamera(ctx context.Context, on bool) error {
	if on == d.IsCameraOn() {
		return nil
	}
	sel := CameraOff
	if on {
		sel = CameraOn
	}
	el := d.waitFor(ctx, sel, 10*time.Second)
	if el == nil {
		return fmt.Errorf("camera button not found (on=%v)", on)
	}
	d.Log.Info("huddle: camera", "on", on)
	return d.click(el)
}

var numRe = regexp.MustCompile(`\d+`)

func (d *Driver) Participants() int {
	el := d.find(MemberCount)
	if el == nil {
		return -1
	}
	txt, err := el.Text()
	if err != nil {
		return -1
	}
	m := numRe.FindString(txt)
	if m == "" {
		return -1
	}
	n, _ := strconv.Atoi(m)
	return n
}

func (d *Driver) waitForBool(ctx context.Context, f func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}

type ProbeButton struct {
	Tag       string `json:"tag"`
	DataQA    string `json:"dataQa"`
	AriaLabel string `json:"ariaLabel"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Visible   bool   `json:"visible"`
}

func (d *Driver) Probe() ([]ProbeButton, error) {
	var out []ProbeButton
	err := d.B.EvalUI(`() => Array.from(document.querySelectorAll('button, [role=button], a[href*="huddle"], input[type=checkbox]')).map(el => ({
		tag: el.tagName.toLowerCase(),
		dataQa: el.getAttribute('data-qa') || '',
		ariaLabel: el.getAttribute('aria-label') || '',
		text: (el.innerText || '').trim().slice(0, 80),
		id: el.id || '',
		visible: !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length),
	}))`, &out)
	return out, err
}

func (d *Driver) waitInHuddle(ctx context.Context, timeout time.Duration) bool {
	return d.waitForBool(ctx, func() bool {
		d.B.AdoptHuddleWindow()
		if el := d.find(Join); el != nil {
			d.Log.Info("huddle: pressing start/join in the huddle window")
			_ = d.click(el)
			time.Sleep(2 * time.Second)
		}
		return d.InHuddle()
	}, timeout)
}

func (d *Driver) pageBroken() bool {
	var title string
	if err := d.B.Eval(`() => document.title`, &title); err != nil {
		return false
	}
	return strings.Contains(title, "Server Error") || strings.Contains(title, "no longer supported")
}
