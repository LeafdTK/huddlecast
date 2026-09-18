package slackrooms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"

var ErrNotLoggedIn = errors.New("slack session cookie rejected (needs re-auth)")

type Client struct {
	host    string
	cookieD string
	http    *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
	convs   []Conversation
	convsAt time.Time
}

func New(host, cookieD string) *Client {
	host = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"), "/")
	return &Client{host: host, cookieD: cookieD, http: &http.Client{Timeout: 30 * time.Second}}
}

type Meeting struct {
	MeetingId         string            `json:"MeetingId"`
	MediaRegion       string            `json:"MediaRegion"`
	ExternalMeetingId string            `json:"ExternalMeetingId"`
	MediaPlacement    map[string]string `json:"MediaPlacement"`
}

type Attendee struct {
	AttendeeId     string            `json:"AttendeeId"`
	ExternalUserId string            `json:"ExternalUserId"`
	JoinToken      string            `json:"JoinToken"`
	Capabilities   map[string]string `json:"Capabilities"`
}

type Join struct {
	CallID       string
	ChannelID    string
	HuddleID     string
	ThreadRootTS string
	Meeting      json.RawMessage
	Attendee     json.RawMessage
	MeetingID    string
	AttendeeID   string
}

type RoomInfo struct {
	ID            string   `json:"id"`
	ThreadRootTS  string   `json:"thread_root_ts"`
	Channels      []string `json:"channels"`
	Participants  []string `json:"participants"`
	ScreenshareOn []string `json:"participants_screenshare_on"`
	CameraOn      []string `json:"participants_camera_on"`
	HuddleLink    string   `json:"huddle_link"`
	HasEnded      bool     `json:"has_ended"`
	CreatedBy     string   `json:"created_by"`
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func (r *RoomInfo) MediaKind(userID string) string {
	if contains(r.ScreenshareOn, userID) {
		return "screen"
	}
	if contains(r.CameraOn, userID) {
		return "camera"
	}
	return ""
}

func (r *RoomInfo) FirstSharing(userIDs []string) (userID, kind string) {
	for _, u := range r.ScreenshareOn {
		if contains(userIDs, u) {
			return u, "screen"
		}
	}
	for _, u := range r.CameraOn {
		if contains(userIDs, u) {
			return u, "camera"
		}
	}
	return "", ""
}

var tokenRe = regexp.MustCompile(`"api_token":"(xoxc-[^"]+)"`)

func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Since(c.tokenAt) < 45*time.Minute {
		tok := c.token
		c.mu.Unlock()
		return tok, nil
	}
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.host+"/", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", "d="+c.cookieD)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	m := tokenRe.FindSubmatch(body)
	if m == nil {
		if bytes.Contains(body, []byte("workspace-signin")) || bytes.Contains(body, []byte("/signin")) {
			return "", ErrNotLoggedIn
		}
		return "", errors.New("no api_token in boot html (cookie may be expired)")
	}
	tok := string(m[1])
	c.mu.Lock()
	c.token, c.tokenAt = tok, time.Now()
	c.mu.Unlock()
	return tok, nil
}

func (c *Client) api(ctx context.Context, method string, fields map[string]string, out any) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("token", tok)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.WriteField("_x_reason", "calls-api/"+method)
	_ = mw.WriteField("_x_mode", "online")
	_ = mw.WriteField("_x_app_name", "client")
	_ = mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+c.host+"/api/"+method, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", "d="+c.cookieD)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	return decode(method, body, out)
}

func decode(method string, body []byte, out any) error {
	var head struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		return fmt.Errorf("%s: bad json: %w", method, err)
	}
	if !head.OK {
		if head.Error == "not_authed" || head.Error == "invalid_auth" || head.Error == "token_expired" {
			return ErrNotLoggedIn
		}
		return fmt.Errorf("%s: %s", method, head.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

func (c *Client) JoinRoom(ctx context.Context, channelID, regions string) (*Join, error) {
	if regions == "" {
		regions = "us-west-1"
	}
	var resp struct {
		Call struct {
			CallID    string `json:"call_id"`
			FreeWilly struct {
				Meeting  json.RawMessage `json:"meeting"`
				Attendee json.RawMessage `json:"attendee"`
			} `json:"free_willy"`
		} `json:"call"`
	}
	if err := c.api(ctx, "rooms.join", map[string]string{
		"channel_id":  channelID,
		"regions":     regions,
		"multidevice": "false",
	}, &resp); err != nil {
		return nil, err
	}
	fw := resp.Call.FreeWilly
	if len(fw.Meeting) == 0 || len(fw.Attendee) == 0 {
		return nil, errors.New("rooms.join: no free_willy media backend in response")
	}
	j := &Join{
		CallID:    resp.Call.CallID,
		ChannelID: channelID,
		Meeting:   fw.Meeting,
		Attendee:  fw.Attendee,
	}
	var m Meeting
	if err := json.Unmarshal(fw.Meeting, &m); err == nil {
		j.MeetingID = m.MeetingId
	}
	var a Attendee
	if err := json.Unmarshal(fw.Attendee, &a); err == nil {
		j.AttendeeID = a.AttendeeId
	}
	return j, nil
}

type Conversation struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) ListConversations(ctx context.Context) ([]Conversation, error) {
	c.mu.Lock()
	if c.convs != nil && time.Since(c.convsAt) < 60*time.Second {
		out := c.convs
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	out, err := c.listConversations(ctx)
	if err != nil {
		out, err = c.listViaCounts(ctx) // enterprise blocks conversations.list; derive from membership
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.convs, c.convsAt = out, time.Now()
	c.mu.Unlock()
	return out, nil
}

func (c *Client) listConversations(ctx context.Context) ([]Conversation, error) {
	var out []Conversation
	cursor := ""
	for {
		var resp struct {
			Channels []Conversation `json:"channels"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.api(ctx, "conversations.list", map[string]string{
			"types":            "public_channel,private_channel",
			"exclude_archived": "true",
			"limit":            "1000",
			"cursor":           cursor,
		}, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Channels...)
		if resp.Meta.NextCursor == "" {
			return out, nil
		}
		cursor = resp.Meta.NextCursor
	}
}

func (c *Client) listViaCounts(ctx context.Context) ([]Conversation, error) {
	var counts struct {
		Channels []struct {
			ID string `json:"id"`
		} `json:"channels"`
	}
	if err := c.api(ctx, "client.counts", nil, &counts); err != nil {
		return nil, err
	}
	out := make([]Conversation, 0, len(counts.Channels))
	for _, ch := range counts.Channels {
		name, _ := c.ConversationName(ctx, ch.ID)
		out = append(out, Conversation{ID: ch.ID, Name: name})
	}
	return out, nil
}

func (c *Client) ConversationName(ctx context.Context, id string) (string, error) {
	var resp struct {
		Channel Conversation `json:"channel"`
	}
	if err := c.api(ctx, "conversations.info", map[string]string{"channel": id}, &resp); err != nil {
		return "", err
	}
	return resp.Channel.Name, nil
}

func (c *Client) RoomInfo(ctx context.Context, roomID string) (*RoomInfo, error) {
	var resp struct {
		Room RoomInfo `json:"room"`
	}
	if err := c.api(ctx, "screenhero.rooms.info", map[string]string{"room": roomID}, &resp); err != nil {
		return nil, err
	}
	return &resp.Room, nil
}
