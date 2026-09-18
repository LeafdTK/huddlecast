package media

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

type keys map[string]bool

func (k keys) StreamKeyExists(_ context.Context, key string) (bool, error) { return k[key], nil }

func TestAuthHandler(t *testing.T) {
	h := AuthHandler(keys{"good": true}, "mtok", slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil)))
	cases := []struct {
		name string
		req  AuthRequest
		want int
	}{
		{"publish known key", AuthRequest{Action: "publish", Path: "live/good"}, 200},
		{"publish unknown key", AuthRequest{Action: "publish", Path: "live/bad"}, 403},
		{"publish with matching bearer", AuthRequest{Action: "publish", Path: "live/good", Token: "good"}, 200},
		{"publish with wrong password", AuthRequest{Action: "publish", Path: "live/good", Password: "nope"}, 403},
		{"read known key", AuthRequest{Action: "read", Path: "live/good"}, 200},
		{"read unknown", AuthRequest{Action: "read", Path: "live/bad"}, 403},
		{"rtmp publish known key", AuthRequest{Action: "publish", Path: "rtmp/good", Protocol: "rtmp"}, 200},
		{"rtmp publish unknown key", AuthRequest{Action: "publish", Path: "rtmp/bad", Protocol: "rtmp"}, 403},
		{"rtmp read by transcoder", AuthRequest{Action: "read", Path: "rtmp/good", Protocol: "rtsp"}, 200},
		{"mirror publish with token", AuthRequest{Action: "publish", Path: "mirror/s1", Token: "mtok"}, 200},
		{"mirror publish without token", AuthRequest{Action: "publish", Path: "mirror/s1"}, 403},
		{"mirror read", AuthRequest{Action: "read", Path: "mirror/s1"}, 200},
		{"other path", AuthRequest{Action: "publish", Path: "random"}, 403},
		{"api", AuthRequest{Action: "api", Path: ""}, 403},
	}
	for _, c := range cases {
		body, _ := json.Marshal(c.req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestURLs(t *testing.T) {
	c := New("http://mtx:9997/", "http://mtx:8889", "example.org")
	if c.WHEPURL(PushPath("k")) != "http://mtx:8889/live/k/whep" {
		t.Error(c.WHEPURL(PushPath("k")))
	}
	if c.WHIPURL(MirrorPath("s")) != "http://mtx:8889/mirror/s/whip" {
		t.Error(c.WHIPURL(MirrorPath("s")))
	}
	if c.PublicWHIPURL("k") != "http://example.org:8889/live/k/whip" {
		t.Error(c.PublicWHIPURL("k"))
	}
	if c.PublicRTMPURL() != "rtmp://example.org:1935/rtmp" {
		t.Error(c.PublicRTMPURL())
	}
}
