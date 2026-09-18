# huddlecast

streams one video source into a bunch of slack huddles at the same time.

OBS pushes to it over WHIP or RTMP, you pick the target channels in the web ui,
and it joins each huddle with a slack account cookie and shares the feed. it can
also mirror an already-running huddle out into other channels.

## run it

    cp huddlecast.example.yml huddlecast.yml   # edit it
    go build -o huddlecast ./cmd/huddlecast
    HUDDLE_COOKIE_1=xoxd-... ./huddlecast serve

needs mediamtx running next to it. web ui and everything else lives on :8080.

secrets (account cookies, stream keys) come from the environment or a `.env`
file, not the yaml. recordings go to disk or an R2 bucket. the whitelist decides
whose screen/camera the mirror is allowed to rebroadcast.
