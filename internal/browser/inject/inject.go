package inject

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed inject.js
var script string

type Config struct {
	WHEPURL      string `json:"whepUrl"`
	Presentation string `json:"presentation"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FPS          int    `json:"fps"`
	Debug        bool   `json:"debug"`
}

func Script(c Config) string {
	b, _ := json.Marshal(c)
	return fmt.Sprintf("window.__HUDDLECAST_CFG = %s;\n%s", b, script)
}

func Raw() string { return script }
