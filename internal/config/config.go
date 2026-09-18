package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen string `yaml:"listen"`

	PublicURL string `yaml:"public_url"`

	DataDir string `yaml:"data_dir"`

	Slack    Slack     `yaml:"slack"`
	Accounts []Account `yaml:"accounts"`
	MediaMTX MediaMTX  `yaml:"mediamtx"`
	Browser  Browser   `yaml:"browser"`
	Huddle   Huddle    `yaml:"huddle"`
	Web      Web       `yaml:"web"`

	Recording Recording `yaml:"recording"`
	Quality   Quality   `yaml:"quality"`
}

type Quality struct {
	Video VideoQuality `yaml:"video"`
	Audio AudioQuality `yaml:"audio"`
}

type VideoQuality struct {
	Width          int `yaml:"width"`
	Height         int `yaml:"height"`
	FPS            int `yaml:"fps"`
	MaxBitrateKbps int `yaml:"max_bitrate_kbps"`
}

type AudioQuality struct {
	Profile        string `yaml:"profile"`
	MaxBitrateKbps int    `yaml:"max_bitrate_kbps"`
}

type Recording struct {
	R2 R2 `yaml:"r2"`
}

type R2 struct {
	Endpoint        string        `yaml:"endpoint"`
	Region          string        `yaml:"region"`
	Bucket          string        `yaml:"bucket"`
	AccessKeyID     string        `yaml:"access_key_id"`
	SecretAccessKey string        `yaml:"secret_access_key"`
	Prefix          string        `yaml:"prefix"`
	PublicBaseURL   string        `yaml:"public_base_url"`
	UploadGrace     time.Duration `yaml:"upload_grace"`
	DeleteLocal     bool          `yaml:"delete_local_after_upload"`
}

func (r R2) Enabled() bool { return r.Bucket != "" && r.Endpoint != "" }

type Slack struct {
	TeamID       string `yaml:"team_id"`
	Host         string `yaml:"host"`
	BotToken     string `yaml:"bot_token"`
	AppToken     string `yaml:"app_token"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`

	Admins []string `yaml:"admins"`

	SlashCommand string `yaml:"slash_command"`
}

func (s Slack) SocketMode() bool { return s.BotToken != "" && s.AppToken != "" }

type Account struct {
	Name string `yaml:"name"`

	CookieD string `yaml:"cookie_d"`

	UserID string `yaml:"user_id"`
}

type MediaMTX struct {
	APIURL string `yaml:"api_url"`

	InternalURL string `yaml:"internal_url"`

	PublicHost string `yaml:"public_host"`

	PublicWHIPBase string `yaml:"public_whip_base"`

	PublicRTMPBase string `yaml:"public_rtmp_base"`

	RecordingsDir string `yaml:"recordings_dir"`
}

type Browser struct {
	Bin string `yaml:"bin"`

	Headless string `yaml:"headless"`

	Width  int `yaml:"width"`
	Height int `yaml:"height"`
	FPS    int `yaml:"fps"`

	ScreenshotInterval time.Duration `yaml:"screenshot_interval"`
}

type Huddle struct {
	JoinTimeout time.Duration `yaml:"join_timeout"`

	LeaveWhenAloneAfter time.Duration `yaml:"leave_when_alone_after"`

	RejoinBackoffMax time.Duration `yaml:"rejoin_backoff_max"`

	MirrorEngine string `yaml:"mirror_engine"`
	TargetEngine string `yaml:"target_engine"`
}

type Web struct {
	Auth string `yaml:"auth"`

	Password string `yaml:"password"`

	SessionSecret string `yaml:"session_secret"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := os.Expand(string(raw), func(k string) string {
		v, ok := os.LookupEnv(k)
		if !ok {
			return "${" + k + "}"
		}
		return v
	})
	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	defD := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.Listen, ":8080")
	def(&c.PublicURL, "http://localhost:8080")
	def(&c.DataDir, "./data")
	def(&c.Slack.SlashCommand, "/huddlecast")
	def(&c.MediaMTX.APIURL, "http://127.0.0.1:9997")
	def(&c.MediaMTX.InternalURL, "http://127.0.0.1:8889")
	def(&c.MediaMTX.PublicHost, "localhost")
	def(&c.MediaMTX.PublicWHIPBase, "http://"+c.MediaMTX.PublicHost+":8889")
	def(&c.MediaMTX.PublicRTMPBase, "rtmp://"+c.MediaMTX.PublicHost+":1935")
	def(&c.MediaMTX.RecordingsDir, "./recordings")
	def(&c.Browser.Headless, "auto")
	if c.Browser.Width == 0 {
		c.Browser.Width = 1280
	}
	if c.Browser.Height == 0 {
		c.Browser.Height = 720
	}
	if c.Browser.FPS == 0 {
		c.Browser.FPS = 30
	}
	defD(&c.Browser.ScreenshotInterval, 5*time.Second)
	defD(&c.Huddle.JoinTimeout, 60*time.Second)
	defD(&c.Huddle.LeaveWhenAloneAfter, 10*time.Minute)
	defD(&c.Huddle.RejoinBackoffMax, 2*time.Minute)
	def(&c.Huddle.MirrorEngine, "chime")
	def(&c.Huddle.TargetEngine, "browser")
	def(&c.Web.Auth, "slack")
	def(&c.Recording.R2.Region, "auto")
	def(&c.Recording.R2.Prefix, "recordings/")
	defD(&c.Recording.R2.UploadGrace, 45*time.Second)
	if c.Quality.Video.Width == 0 {
		c.Quality.Video.Width = c.Browser.Width
	}
	if c.Quality.Video.Height == 0 {
		c.Quality.Video.Height = c.Browser.Height
	}
	if c.Quality.Video.FPS == 0 {
		c.Quality.Video.FPS = c.Browser.FPS
	}
	if c.Quality.Video.MaxBitrateKbps == 0 {
		c.Quality.Video.MaxBitrateKbps = 2500
	}
	def(&c.Quality.Audio.Profile, "music-stereo")
	if c.Quality.Audio.MaxBitrateKbps == 0 {
		c.Quality.Audio.MaxBitrateKbps = 128
	}
}

func (c *Config) validate() error {
	var errs []error
	unexpanded := func(field, v string) {
		if strings.Contains(v, "${") {
			errs = append(errs, fmt.Errorf("%s references an unset environment variable: %s", field, v))
		}
	}
	unexpanded("slack.bot_token", c.Slack.BotToken)
	unexpanded("slack.app_token", c.Slack.AppToken)
	unexpanded("web.session_secret", c.Web.SessionSecret)
	if c.Slack.TeamID == "" {
		errs = append(errs, errors.New("slack.team_id is required"))
	}
	if len(c.Accounts) == 0 {
		errs = append(errs, errors.New("at least one huddle account is required (accounts:)"))
	}
	seen := map[string]bool{}
	for i, a := range c.Accounts {
		if a.Name == "" {
			errs = append(errs, fmt.Errorf("accounts[%d].name is required", i))
		}
		if seen[a.Name] {
			errs = append(errs, fmt.Errorf("duplicate account name %q", a.Name))
		}
		seen[a.Name] = true
		unexpanded("accounts."+a.Name+".cookie_d", a.CookieD)
	}
	switch c.Web.Auth {
	case "slack":
		if c.Slack.ClientID == "" || c.Slack.ClientSecret == "" {
			errs = append(errs, errors.New("web.auth=slack requires slack.client_id and slack.client_secret"))
		}
	case "password":
		if c.Web.Password == "" {
			errs = append(errs, errors.New("web.auth=password requires web.password"))
		}
	default:
		errs = append(errs, fmt.Errorf("web.auth must be slack or password, got %q", c.Web.Auth))
	}
	if len(c.Web.SessionSecret) < 16 {
		errs = append(errs, errors.New("web.session_secret must be at least 16 characters"))
	}
	switch c.Browser.Headless {
	case "auto", "true", "false":
	default:
		errs = append(errs, fmt.Errorf("browser.headless must be auto/true/false, got %q", c.Browser.Headless))
	}
	switch c.Quality.Audio.Profile {
	case "music-stereo", "music-mono", "speech":
	default:
		errs = append(errs, fmt.Errorf("quality.audio.profile must be music-stereo/music-mono/speech, got %q", c.Quality.Audio.Profile))
	}
	if c.Recording.R2.Bucket != "" {
		unexpanded("recording.r2.access_key_id", c.Recording.R2.AccessKeyID)
		unexpanded("recording.r2.secret_access_key", c.Recording.R2.SecretAccessKey)
		if c.Recording.R2.Endpoint == "" {
			errs = append(errs, errors.New("recording.r2.endpoint is required when a bucket is set"))
		}
		if c.Recording.R2.AccessKeyID == "" || c.Recording.R2.SecretAccessKey == "" {
			errs = append(errs, errors.New("recording.r2 requires access_key_id and secret_access_key"))
		}
	}
	return errors.Join(errs...)
}

func (c *Config) Account(name string) (Account, bool) {
	for _, a := range c.Accounts {
		if a.Name == name {
			return a, true
		}
	}
	return Account{}, false
}
