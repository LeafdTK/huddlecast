package media

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	apiURL      string
	internalURL string
	publicHost  string
	whipBase    string
	rtmpBase    string
	http        *http.Client
}

func New(apiURL, internalURL, publicHost, whipBase, rtmpBase string) *Client {
	if whipBase == "" {
		whipBase = fmt.Sprintf("http://%s:8889", publicHost)
	}
	if rtmpBase == "" {
		rtmpBase = fmt.Sprintf("rtmp://%s:1935", publicHost)
	}
	return &Client{
		apiURL:      strings.TrimRight(apiURL, "/"),
		internalURL: strings.TrimRight(internalURL, "/"),
		publicHost:  publicHost,
		whipBase:    strings.TrimRight(whipBase, "/"),
		rtmpBase:    strings.TrimRight(rtmpBase, "/"),
		http:        &http.Client{Timeout: 5 * time.Second},
	}
}

func PushPath(streamKey string) string { return "live/" + streamKey }

func RTMPPath(streamKey string) string { return "rtmp/" + streamKey }

func MirrorPath(sessionID string) string { return "mirror/" + sessionID }

func (c *Client) WHEPURL(path string) string { return c.internalURL + "/" + path + "/whep" }

func (c *Client) WHIPURL(path string) string { return c.internalURL + "/" + path + "/whip" }

func (c *Client) PublicWHIPURL(streamKey string) string {
	return fmt.Sprintf("%s/%s/whip", c.whipBase, PushPath(streamKey))
}

func (c *Client) PublicRTMPURL() string {
	return c.rtmpBase + "/rtmp"
}

func (c *Client) PublicWHEPURL(path string) string {
	return fmt.Sprintf("%s/%s/whep", c.whipBase, path)
}

type Path struct {
	Name      string `json:"name"`
	Ready     bool   `json:"ready"`
	Online    bool   `json:"online"`
	Available bool   `json:"available"`
	Source    *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"source"`
	Readers []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"readers"`
	Tracks []string `json:"tracks"`
}

func (p Path) IsLive() bool { return p.Ready || p.Online || p.Available }

func (c *Client) SetRecord(ctx context.Context, name string, record bool) error {
	body := fmt.Sprintf(`{"record": %t}`, record)
	esc := url.PathEscape(name)
	err := c.send(ctx, http.MethodPatch, "/v3/config/paths/patch/"+esc, body)
	if err != nil && strings.Contains(err.Error(), "404") {
		return c.send(ctx, http.MethodPost, "/v3/config/paths/add/"+esc, body)
	}
	return err
}

func (c *Client) send(ctx context.Context, method, path, body string) error {
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("mediamtx %s %s: %d", method, path, resp.StatusCode)
	}
	return nil
}

func (c *Client) ListPaths(ctx context.Context) ([]Path, error) {
	var out struct {
		Items []Path `json:"items"`
	}
	if err := c.get(ctx, "/v3/paths/list?itemsPerPage=1000", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *Client) GetPath(ctx context.Context, name string) (*Path, error) {
	var p Path
	err := c.get(ctx, "/v3/paths/get/"+url.PathEscape(name), &p)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

type Recording struct {
	Name     string `json:"name"`
	Segments []struct {
		Start string `json:"start"`
	} `json:"segments"`
}

func (c *Client) ListRecordings(ctx context.Context) ([]Recording, error) {
	var out struct {
		Items []Recording `json:"items"`
	}
	if err := c.get(ctx, "/v3/recordings/list?itemsPerPage=1000", &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func (c *Client) Healthy(ctx context.Context) error {
	_, err := c.ListPaths(ctx)
	return err
}

func (c *Client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mediamtx api: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("mediamtx api %s: status %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(v)
}
