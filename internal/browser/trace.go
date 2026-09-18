package browser

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

type TraceEntry struct {
	Page     string            `json:"page"`
	At       string            `json:"at"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	PostData string            `json:"postData,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Status   int               `json:"status,omitempty"`
	Body     string            `json:"body,omitempty"`
}

type tracer struct {
	f     *os.File
	mu    sync.Mutex
	match func(string) bool
}

func (t *tracer) write(e *TraceEntry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	line, _ := json.Marshal(e)
	t.f.Write(append(line, '\n'))
}

func (b *Browser) Trace(path string, match func(url string) bool) (stop func()) {
	f, err := os.Create(path)
	if err != nil {
		b.log.Error("trace: open", "err", err)
		return func() {}
	}
	b.tracer = &tracer{f: f, match: match}
	b.tracePage(b.page, "main")
	return func() { f.Close() }
}

func (b *Browser) tracePage(page *rod.Page, label string) {
	t := b.tracer
	if t == nil {
		return
	}
	match := t.match
	pending := map[proto.NetworkRequestID]*TraceEntry{}
	var mu sync.Mutex
	write := func(e *TraceEntry) {
		e.Page = label
		t.write(e)
	}
	_ = proto.RuntimeEnable{}.Call(page)
	_, _ = page.EvalOnNewDocument(`(() => { const o = window.open; window.open = function(...a) { console.log("[huddlecast] window.open", String(a[0]), String(a[1])); const w = o.apply(this, a); console.log("[huddlecast] window.open ->", w ? "ok" : "blocked"); return w; }; })()`)
	go page.EachEvent(func(e *proto.NetworkRequestWillBeSent) {
		if !match(e.Request.URL) {
			return
		}
		h := map[string]string{}
		for k, v := range e.Request.Headers {
			ks := strings.ToLower(k)
			if ks == "content-type" || ks == "authorization" || ks == "x-slack-version-ts" {
				h[ks] = v.String()
			}
		}
		mu.Lock()
		pending[e.RequestID] = &TraceEntry{At: time.Now().Format(time.RFC3339Nano), Method: e.Request.Method, URL: e.Request.URL, PostData: e.Request.PostData, Headers: h}
		mu.Unlock()
	}, func(e *proto.NetworkResponseReceived) {
		mu.Lock()
		te := pending[e.RequestID]
		mu.Unlock()
		if te != nil {
			te.Status = e.Response.Status
		}
	}, func(e *proto.RuntimeConsoleAPICalled) {
		var parts []string
		for _, a := range e.Args {
			if a.Value.Nil() {
				parts = append(parts, a.Description)
			} else {
				parts = append(parts, a.Value.String())
			}
		}
		write(&TraceEntry{At: time.Now().Format(time.RFC3339Nano), Method: "console." + string(e.Type), Body: strings.Join(parts, " ")})
	}, func(e *proto.RuntimeExceptionThrown) {
		write(&TraceEntry{At: time.Now().Format(time.RFC3339Nano), Method: "exception", Body: e.ExceptionDetails.Text + " " + e.ExceptionDetails.Exception.Description})
	}, func(e *proto.NetworkLoadingFinished) {
		mu.Lock()
		te := pending[e.RequestID]
		delete(pending, e.RequestID)
		mu.Unlock()
		if te == nil {
			return
		}
		res, err := (proto.NetworkGetResponseBody{RequestID: e.RequestID}).Call(page)
		if err == nil {
			te.Body = res.Body
			if len(te.Body) > 200000 {
				te.Body = te.Body[:200000] + "…"
			}
		}
		write(te)
	})()
}
