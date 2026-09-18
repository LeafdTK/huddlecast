package huddle

type Selector struct {
	CSS []string

	TextRegexp string
}

var (
	Join = Selector{
		CSS: []string{
			`[data-qa="huddle_invite_window_with_preview_content_join_button"]`,
			`[data-qa="huddle_join_button"]`,
			`[data-qa="huddle-join-button"]`,
			`[data-qa="huddle_prejoin_join_button"]`,
			`button[data-qa*="join" i][data-qa*="huddle" i]`,
			`button[aria-label*="join huddle" i]`,
			`button[aria-label*="join" i]:not([aria-label*="channel" i])`,
		},
		TextRegexp: `(?i)^\s*(join|join huddle|join now|start huddle)\s*$`,
	}

	Toggle = Selector{
		CSS: []string{
			`[data-qa="huddle_channel_header_button__join_button"]`,
			`[data-qa="huddle_channel_header_button__start_button"]`,
			`#huddle_toggle`,
			`[data-qa="huddle_toggle"]`,
			`button[aria-label*="huddle" i][aria-label*="join" i]`,
			`button[aria-label*="huddle" i][aria-label*="start" i]`,
		},
	}

	Leave = Selector{
		CSS: []string{
			`[data-qa="huddle_leave_button"]`,
			`[data-qa="huddle_mini_player_leave_button"]`,
			`button[aria-label*="leave huddle" i]`,
			`button[aria-label*="leave" i]`,
		},
		TextRegexp: `(?i)^\s*leave\s*$`,
	}

	Unmute = Selector{
		CSS: []string{`[data-qa="huddle_unmute_button"]`, `button[aria-label*="unmute" i]`, `button[aria-label*="turn on microphone" i]`},
	}
	Mute = Selector{
		CSS: []string{`[data-qa="huddle_mute_button"]`, `button[aria-label^="mute" i]`, `button[aria-label*="turn off microphone" i]`},
	}

	ShareScreen = Selector{
		CSS: []string{
			`[data-qa="huddle_share_screen_button"]`,
			`[data-qa="huddle_screen_share_button"]`,
			`button[aria-label*="share screen" i]`,
			`button[aria-label*="share your screen" i]`,
			`button[aria-label*="start sharing" i]`,
			`button[aria-label*="screen" i]:not([aria-label*="stop" i])`,
		},
		TextRegexp: `(?i)share\s+(your\s+)?screen`,
	}
	StopShare = Selector{
		CSS: []string{`[data-qa="huddle_stop_share_screen_button"]`, `button[aria-label*="stop sharing" i]`, `button[aria-label*="stop share" i]`},
	}

	CameraOn = Selector{
		CSS: []string{
			`[data-qa="huddle_video_on_button"]`,
			`button[aria-label*="turn on video" i]`,
			`button[aria-label*="turn on camera" i]`,
			`button[aria-label*="start video" i]`,
		},
	}
	CameraOff = Selector{
		CSS: []string{
			`[data-qa="huddle_video_off_button"]`,
			`button[aria-label*="turn off video" i]`,
			`button[aria-label*="turn off camera" i]`,
			`button[aria-label*="stop video" i]`,
		},
	}

	MemberCount = Selector{
		CSS: []string{
			`.p-huddle_activity__member_count_text`,
			`[data-qa="huddle_member_count"]`,
			`[data-qa*="participant_count" i]`,
			`[data-qa*="member_count" i]`,
		},
	}

	AccessDenied = Selector{
		CSS:        []string{`[data-qa="huddle_request_access_button"]`, `button[aria-label*="request access" i]`},
		TextRegexp: `(?i)request (access|to join)`,
	}
)

func DeepLinkURL(teamID, channelID string) string {
	return "https://app.slack.com/huddle/" + teamID + "/" + channelID
}

func ClientURL(teamID, channelID string) string {
	return "https://app.slack.com/client/" + teamID + "/" + channelID
}
