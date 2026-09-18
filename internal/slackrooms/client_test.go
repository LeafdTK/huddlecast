package slackrooms

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const bootHTML = `<!doctype html><html><body><script>var boot = {"api_token":"xoxc-1-2-3-deadbeef","team_id":"E09"};</script></body></html>`

const joinBody = `{"ok":true,"call":{"call_id":"R0C2K286NJZ","free_willy":{"meeting":{"MeetingId":"e2c23a36","MediaRegion":"us-west-1","ExternalMeetingId":"E-R-U","MediaPlacement":{"SignalingUrl":"wss://signal.z2.uw1.m.chime.aws/control/e2c23a36"}},"attendee":{"AttendeeId":"feb5eb88","ExternalUserId":"E-R-U","JoinToken":"ZmVi","Capabilities":{"Audio":"SendReceive"}}}}}`

const infoBody = `{"ok":true,"room":{"id":"R0C2K286NJZ","thread_root_ts":"1789677986.948749","channels":["C0C2RBUP9TK"],"participants":["U0C283HACQP"],"huddle_link":"https://app.slack.com/huddle/E09/C0C2RBUP9TK","has_ended":false,"created_by":"U0C283HACQP"}}`

func testServer(t *testing.T) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			io.WriteString(w, bootHTML)
		case strings.HasSuffix(r.URL.Path, "/api/rooms.join"):
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse form: %v", err)
			}
			if r.FormValue("token") != "xoxc-1-2-3-deadbeef" {
				t.Errorf("token not forwarded: %q", r.FormValue("token"))
			}
			io.WriteString(w, joinBody)
		case strings.HasSuffix(r.URL.Path, "/api/screenhero.rooms.info"):
			io.WriteString(w, infoBody)
		default:
			http.Error(w, `{"ok":false,"error":"unknown_method"}`, 404)
		}
	}))
	c := New(srv.URL, "xoxd-fake")
	c.http = srv.Client()
	return srv, c
}

func TestJoinRoom(t *testing.T) {
	srv, c := testServer(t)
	defer srv.Close()

	j, err := c.JoinRoom(context.Background(), "C0C2RBUP9TK", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.CallID != "R0C2K286NJZ" || j.MeetingID != "e2c23a36" || j.AttendeeID != "feb5eb88" {
		t.Fatalf("bad join: %+v", j)
	}
	if len(j.Meeting) == 0 || len(j.Attendee) == 0 {
		t.Fatal("raw meeting/attendee not preserved for chime")
	}
	if !strings.Contains(string(j.Meeting), "SignalingUrl") {
		t.Errorf("meeting raw missing SignalingUrl: %s", j.Meeting)
	}
}

func TestRoomInfo(t *testing.T) {
	srv, c := testServer(t)
	defer srv.Close()

	info, err := c.RoomInfo(context.Background(), "R0C2K286NJZ")
	if err != nil {
		t.Fatal(err)
	}
	if info.ThreadRootTS != "1789677986.948749" || len(info.Channels) != 1 || info.Channels[0] != "C0C2RBUP9TK" {
		t.Fatalf("bad room info: %+v", info)
	}
}

func TestTokenCached(t *testing.T) {
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		io.WriteString(w, bootHTML)
	}))
	defer srv.Close()
	c := New(srv.URL, "xoxd-fake")
	c.http = srv.Client()
	for i := 0; i < 3; i++ {
		if _, err := c.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if hits != 1 {
		t.Errorf("token not cached: %d boot fetches", hits)
	}
}

func TestNotLoggedIn(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<html><body><a href="/signin?redir=x">workspace-signin</a></body></html>`)
	}))
	defer srv.Close()
	c := New(srv.URL, "xoxd-expired")
	c.http = srv.Client()
	if _, err := c.Token(context.Background()); err != ErrNotLoggedIn {
		t.Fatalf("want ErrNotLoggedIn, got %v", err)
	}
}

func TestMediaKind(t *testing.T) {
	r := &RoomInfo{ScreenshareOn: []string{"U1"}, CameraOn: []string{"U2"}}
	if r.MediaKind("U1") != "screen" {
		t.Error("U1 should be screen")
	}
	if r.MediaKind("U2") != "camera" {
		t.Error("U2 should be camera")
	}
	if r.MediaKind("U3") != "" {
		t.Error("U3 shares nothing")
	}
	if u, k := r.FirstSharing([]string{"U9", "U2"}); u != "U2" || k != "camera" {
		t.Errorf("first sharing whitelisted: %s %s", u, k)
	}
	if u, k := r.FirstSharing([]string{"U2", "U1"}); u != "U1" || k != "screen" {
		t.Errorf("screen should win over camera: %s %s", u, k)
	}
}
