package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"

	"github.com/LeafdTK/huddlecast/internal/browser/inject"
)

type Options struct {
	Bin        string
	Headless   string
	ProfileDir string
	Width      int
	Height     int
	Log        *slog.Logger
}

type Browser struct {
	opts    Options
	browser *rod.Browser
	page    *rod.Page
	ui      *rod.Page
	shim    string
	log     *slog.Logger
	onOpen  func()
	tracer  *tracer
}

var ErrNotLoggedIn = errors.New("slack session cookie rejected (needs re-auth)")

func Launch(ctx context.Context, opts Options) (*Browser, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if err := os.MkdirAll(opts.ProfileDir, 0o755); err != nil {
		return nil, err
	}
	for _, f := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(opts.ProfileDir, f))
	}
	l := launcher.New().
		UserDataDir(opts.ProfileDir).
		Leakless(false).
		Set("use-fake-ui-for-media-stream").
		Set("use-fake-device-for-media-stream").
		Set("autoplay-policy", "no-user-gesture-required").
		Set("disable-dev-shm-usage").
		Set("disable-background-timer-throttling").
		Set("disable-renderer-backgrounding").
		Set("disable-backgrounding-occluded-windows").
		Set("window-size", fmt.Sprintf("%d,%d", opts.Width, opts.Height+120)).
		Set("lang", "en-US").
		Set("allow-running-insecure-content").
		Set("disable-features", "BlockInsecurePrivateNetworkRequests,PrivateNetworkAccessSendPreflights,PrivateNetworkAccessRespectPreflightResults,LocalNetworkAccessChecks,LocalNetworkAccessForWorkers")
	if opts.Bin == "" {
		if p, ok := launcher.LookPath(); ok {
			opts.Bin = p
		}
	}
	if opts.Bin != "" {
		l = l.Bin(opts.Bin)
	}
	l = l.Set("user-agent", userAgent(opts.Bin))
	if os.Geteuid() == 0 || os.Getenv("HUDDLECAST_NO_SANDBOX") != "" {
		l = l.NoSandbox(true).Set("disable-gpu")
	} else {
		// keep the GPU on and let it decode video in hardware where possible
		l = l.Set("ignore-gpu-blocklist").Set("enable-accelerated-video-decode").Set("enable-zero-copy")
	}
	headless := resolveHeadless(opts.Headless)
	if headless {
		l = l.HeadlessNew(true)
	} else {
		l = l.Headless(false)
		if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" {

			l = l.XVFB(fmt.Sprintf("--server-args=-screen 0 %dx%dx24", opts.Width, opts.Height+120))
		}
	}
	u, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("launch chromium: %w", err)
	}
	b := rod.New().ControlURL(u).Context(ctx)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("connect chromium: %w", err)
	}
	return &Browser{opts: opts, browser: b, log: opts.Log}, nil
}

func chromeVersion(bin string) (major, full string) {
	major, full = "128", "128.0.0.0"
	if bin != "" {
		if out, err := exec.Command(bin, "--version").Output(); err == nil {
			if m := regexp.MustCompile(`(\d+)\.\d+\.\d+\.\d+`).FindStringSubmatch(string(out)); m != nil {
				major, full = m[1], m[0]
			}
		}
	}
	return
}

func userAgent(bin string) string {
	major, _ := chromeVersion(bin)
	platform := "X11; Linux x86_64"
	if runtime.GOOS == "darwin" {
		platform = "Macintosh; Intel Mac OS X 10_15_7"
	}
	return fmt.Sprintf("Mozilla/5.0 (%s) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s.0.0.0 Safari/537.36", platform, major)
}

func uaOverride(bin string) *proto.NetworkSetUserAgentOverride {
	major, full := chromeVersion(bin)
	platform, platformVersion, arch := "Linux", "6.1.0", "x86"
	if runtime.GOOS == "darwin" {
		platform, platformVersion, arch = "macOS", "14.0.0", "arm"
	}
	return &proto.NetworkSetUserAgentOverride{
		UserAgent:      userAgent(bin),
		AcceptLanguage: "en-US,en;q=0.9",
		Platform:       platform,
		UserAgentMetadata: &proto.EmulationUserAgentMetadata{
			Brands: []*proto.EmulationUserAgentBrandVersion{
				{Brand: "Chromium", Version: major},
				{Brand: "Google Chrome", Version: major},
				{Brand: "Not_A Brand", Version: "24"},
			},
			FullVersionList: []*proto.EmulationUserAgentBrandVersion{
				{Brand: "Chromium", Version: full},
				{Brand: "Google Chrome", Version: full},
				{Brand: "Not_A Brand", Version: "24.0.0.0"},
			},
			Platform: platform, PlatformVersion: platformVersion, Architecture: arch, Model: "", Mobile: false, Bitness: "64",
		},
	}
}

func resolveHeadless(mode string) bool {
	switch mode {
	case "true":
		return true
	case "false":
		return false
	default:

		return runtime.GOOS != "linux"
	}
}

func (b *Browser) OpenSlack(ctx context.Context, cookieD string, shim inject.Config, url string) error {
	page, err := b.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return err
	}
	page = page.Context(ctx)
	b.page = page
	if b.onOpen != nil {
		b.onOpen()
	}
	if err := page.SetCookies([]*proto.NetworkCookieParam{{
		Name: "d", Value: cookieD, Domain: ".slack.com", Path: "/", Secure: true, HTTPOnly: true,
		SameSite: proto.NetworkCookieSameSiteLax,
	}}); err != nil {
		return fmt.Errorf("set cookie: %w", err)
	}
	if err := page.SetUserAgent(uaOverride(b.opts.Bin)); err != nil {
		return fmt.Errorf("set user agent: %w", err)
	}
	if _, err := page.EvalOnNewDocument(stealth.JS); err != nil {
		return fmt.Errorf("install stealth: %w", err)
	}
	if shim.Presentation != "off" {
		b.shim = inject.Script(shim)
		if _, err := page.EvalOnNewDocument(b.shim); err != nil {
			return fmt.Errorf("install shim: %w", err)
		}
	}
	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: b.opts.Width, Height: b.opts.Height + 120, DeviceScaleFactor: 1,
	}); err != nil {
		return err
	}
	if err := page.Navigate(url); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	if err := page.Timeout(45 * time.Second).WaitLoad(); err != nil {
		b.log.Warn("page load wait timed out, continuing", "err", err)
	}
	return b.CheckLoggedIn()
}

func (b *Browser) OpenPage(ctx context.Context, url, initScript string) error {
	page, err := b.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return err
	}
	page = page.Context(ctx)
	b.page = page
	_ = proto.NetworkEnable{}.Call(page)
	_ = (proto.NetworkSetCacheDisabled{CacheDisabled: true}).Call(page)
	if initScript != "" {
		if _, err := page.EvalOnNewDocument(initScript); err != nil {
			return fmt.Errorf("inject init script: %w", err)
		}
	}
	if err := page.SetViewport(&proto.EmulationSetDeviceMetricsOverride{
		Width: b.opts.Width, Height: b.opts.Height, DeviceScaleFactor: 1,
	}); err != nil {
		return err
	}
	if err := page.Navigate(url); err != nil {
		return fmt.Errorf("navigate: %w", err)
	}
	if err := page.Timeout(45 * time.Second).WaitLoad(); err != nil {
		b.log.Warn("page load wait timed out, continuing", "err", err)
	}
	return nil
}

func (b *Browser) CheckLoggedIn() error {
	info, err := b.page.Info()
	if err != nil {
		return err
	}
	u := info.URL
	if strings.Contains(u, "signin") || strings.Contains(u, "/sign_in") || strings.Contains(u, "workspace-signin") || strings.Contains(u, "slack.com/get-started") {
		return ErrNotLoggedIn
	}
	return nil
}

func (b *Browser) Page() *rod.Page {
	if b.ui != nil {
		return b.ui
	}
	return b.page
}

func (b *Browser) MainPage() *rod.Page { return b.page }

func (b *Browser) HasHuddleWindow() bool { return b.ui != nil }

func (b *Browser) AdoptHuddleWindow() bool {
	pages, err := b.browser.Pages()
	if err != nil {
		return false
	}
	if b.ui != nil {
		alive := false
		for _, p := range pages {
			if p.TargetID == b.ui.TargetID {
				alive = true
			}
		}
		if !alive {
			b.log.Info("huddle window closed")
			b.ui = nil
		}
	}
	for _, p := range pages {
		if p.TargetID == b.page.TargetID || (b.ui != nil && p.TargetID == b.ui.TargetID) {
			continue
		}
		info, err := p.Info()
		if err != nil || info.Type != "page" {
			continue
		}
		child := info.OpenerID == b.page.TargetID
		if !child && !strings.Contains(info.URL, "huddle") && !strings.Contains(info.URL, "free-willy") {
			continue
		}
		b.ui = p.Context(b.page.GetContext())
		b.tracePage(b.ui, "huddle")
		b.log.Info("adopted huddle window", "url", info.URL, "child", child)
		return true
	}
	return false
}

func (b *Browser) PageURLs() []string {
	pages, err := b.browser.Pages()
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range pages {
		if info, err := p.Info(); err == nil {
			out = append(out, string(info.Type)+" "+info.URL+" opener="+string(info.OpenerID))
		}
	}
	return out
}

func (b *Browser) InstallScript(js string) error {
	for _, p := range []*rod.Page{b.page, b.ui} {
		if p == nil {
			continue
		}
		if _, err := p.EvalOnNewDocument(js); err != nil {
			return err
		}
		if _, err := p.Eval(js); err != nil {
			return err
		}
	}
	return nil
}

func (b *Browser) URL() string {
	info, err := b.page.Info()
	if err != nil {
		return ""
	}
	return info.URL
}

func (b *Browser) Screenshot() ([]byte, error) {
	q := 60
	return b.Page().Screenshot(false, &proto.PageCaptureScreenshot{Format: proto.PageCaptureScreenshotFormatJpeg, Quality: &q})
}

func (b *Browser) Eval(js string, v any) error {
	res, err := b.page.Eval(js)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return res.Value.Unmarshal(v)
}

func (b *Browser) EvalUI(js string, v any) error {
	res, err := b.Page().Eval(js)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return res.Value.Unmarshal(v)
}

type ShimState struct {
	Status        string `json:"status"`
	Error         string `json:"error"`
	WHEPConnects  int    `json:"whepConnects"`
	LastFrameAt   int64  `json:"lastFrameAt"`
	FramesPainted int64  `json:"framesPainted"`
	Presentation  string `json:"presentation"`
	RemoteTracks  struct {
		Audio int `json:"audio"`
		Video int `json:"video"`
	} `json:"remoteTracks"`
	HandedOut struct {
		GetUserMedia    int `json:"getUserMedia"`
		GetDisplayMedia int `json:"getDisplayMedia"`
	} `json:"handedOut"`
	VideoReady int    `json:"videoReady"`
	AudioCtx   string `json:"audioCtx"`
}

func (b *Browser) ShimState() (*ShimState, error) {
	var s ShimState
	if err := b.Eval(`() => window.__huddlecast ? window.__huddlecast.snapshot() : null`, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (b *Browser) SetPresentation(p string) error {
	_, err := b.page.Eval(`(p) => window.__huddlecast && window.__huddlecast.setPresentation(p)`, p)
	return err
}

func (b *Browser) SaveDebug(dir, label string) {
	_ = os.MkdirAll(dir, 0o755)
	ts := time.Now().Format("20060102-150405")
	if img, err := b.Screenshot(); err == nil {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%s.jpg", ts, label)), img, 0o644)
	}
	if html, err := b.Page().HTML(); err == nil {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%s.html", ts, label)), []byte(html), 0o644)
	}
}

func (b *Browser) TraceOnOpen(path string, match func(string) bool) (stop func()) {
	stop = func() {}
	b.onOpen = func() { stop = b.Trace(path, match) }
	return func() { stop() }
}

func (b *Browser) Close() error {
	if b.browser == nil {
		return nil
	}
	return b.browser.Close()
}
