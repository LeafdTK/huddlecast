package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"

	"github.com/LeafdTK/huddlecast/internal/browser/inject"
)

const fixture = `<!doctype html><html><body><h1>fixture</h1><script>
window.results = {};
(async () => {
  try {
    const ch = navigator.userAgentData ? await navigator.userAgentData.getHighEntropyValues(["fullVersionList"]) : {};
    const gum = await navigator.mediaDevices.getUserMedia({audio: true, video: true});
    const gdm = await navigator.mediaDevices.getDisplayMedia({video: true, audio: true});
    const devs = await navigator.mediaDevices.enumerateDevices();
    window.results = {
      gum: gum.getTracks().map(t => t.kind + ":" + t.readyState),
      gdm: gdm.getTracks().map(t => t.kind + ":" + t.readyState),
      devices: devs.map(d => d.kind),
      settings: gdm.getVideoTracks()[0].getSettings(),
      ua: navigator.userAgent,
      brands: (ch.brands || []).map(b => b.brand),
      webdriver: navigator.webdriver,
    };
  } catch (e) { window.results = {error: String(e)}; }
})();
</script></body></html>`

func findBrowser(t *testing.T) string {
	t.Helper()
	if os.Getenv("HUDDLECAST_SKIP_BROWSER_TESTS") != "" {
		t.Skip("browser tests disabled")
	}
	if p, ok := launcher.LookPath(); ok {
		return p
	}
	t.Skip("no chromium found; set HUDDLECAST_SKIP_BROWSER_TESTS=1 to silence")
	return ""
}

func TestShimReplacesMediaDevices(t *testing.T) {
	bin := findBrowser(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(fixture))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := Launch(ctx, Options{Bin: bin, Headless: "true", ProfileDir: filepath.Join(t.TempDir(), "p"), Width: 640, Height: 360})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	shim := inject.Config{WHEPURL: os.Getenv("HUDDLECAST_TEST_WHEP"), Presentation: "screen", Width: 640, Height: 360, FPS: 15, Debug: true}
	if err := b.OpenSlack(ctx, "fake-cookie", shim, srv.URL); err != nil {
		t.Fatal(err)
	}

	var res struct {
		Error     string         `json:"error"`
		GUM       []string       `json:"gum"`
		GDM       []string       `json:"gdm"`
		Devices   []string       `json:"devices"`
		Settings  map[string]any `json:"settings"`
		UA        string         `json:"ua"`
		Brands    []string       `json:"brands"`
		Webdriver bool           `json:"webdriver"`
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := b.Eval(`() => window.results`, &res); err == nil && (res.Error != "" || len(res.GDM) > 0) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if res.Error != "" {
		t.Fatalf("fixture error: %s", res.Error)
	}
	if strings.Contains(res.UA, "Headless") || strings.Contains(strings.Join(res.Brands, ","), "Headless") || res.Webdriver {
		t.Errorf("headless fingerprint leaks: ua=%q brands=%v webdriver=%v", res.UA, res.Brands, res.Webdriver)
	}
	if len(res.GUM) != 2 || len(res.GDM) != 2 {
		t.Fatalf("expected audio+video from both, got gum=%v gdm=%v", res.GUM, res.GDM)
	}
	for _, tr := range append(res.GUM, res.GDM...) {
		if tr != "audio:live" && tr != "video:live" {
			t.Errorf("track not live: %s", tr)
		}
	}
	if len(res.Devices) == 0 {
		t.Errorf("enumerateDevices: %v", res.Devices)
	}
	if w, _ := res.Settings["width"].(float64); int(w) != 640 {
		t.Errorf("display track width = %v, want 640", res.Settings["width"])
	}
	st, err := b.ShimState()
	if err != nil {
		t.Fatal(err)
	}
	if st.HandedOut.GetDisplayMedia != 1 || st.HandedOut.GetUserMedia != 1 {
		t.Errorf("handedOut = %+v", st.HandedOut)
	}
	if st.AudioCtx != "running" {
		t.Errorf("audio context state = %s", st.AudioCtx)
	}
	if shim.WHEPURL != "" {
		time.Sleep(6 * time.Second)
		st, _ = b.ShimState()
		if st.Status != "live" || st.FramesPainted == 0 {
			t.Errorf("WHEP not live: %+v", st)
		}
		if st.RemoteTracks.Audio == 0 || st.RemoteTracks.Video == 0 {
			t.Errorf("WHEP should deliver audio and video, got %+v", st.RemoteTracks)
		}
	} else if st.Status != "error" {
		t.Errorf("without WHEP the shim should report error, got %q", st.Status)
	}
	if err := b.SetPresentation("camera"); err != nil {
		t.Fatal(err)
	}
	st, _ = b.ShimState()
	if st.Presentation != "camera" {
		t.Errorf("presentation switch failed: %+v", st)
	}
	if img, err := b.Screenshot(); err != nil || len(img) < 100 {
		t.Errorf("screenshot: %v (%d bytes)", err, len(img))
	}
}
