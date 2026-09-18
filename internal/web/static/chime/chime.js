(() => {
  if (window.__huddlecastChime) return;
  const cfg = Object.assign(
    { mode: "receive", meeting: null, attendee: null, targetUserIDs: [], kind: "", whipUrl: "", whepUrl: "", token: "", width: 1280, height: 720, fps: 30, debug: false },
    window.__HUDDLECAST_CHIME_CFG || {}
  );
  const log = (...a) => cfg.debug && console.log("[huddlecast-chime]", ...a);
  const statusEl = document.getElementById("hc-status");
  const state = {
    mode: cfg.mode, status: "init", error: null, joined: false, contentShare: false, standby: false, audio: false, audioProfile: "", micLevel: 0, micMuted: null, pcState: "", dropReason: "", tracks: "",
    srcRes: "", srcFps: 0, srcKbps: 0, paintFps: 0, targetFps: cfg.fps, framesDropped: 0, dropRate: 0, sourceVideo: "", framesPainted: 0,
    targetUser: "", targetKind: "", sourceVideo: "", tiles: 0, whepConnects: 0, publishes: 0,
    framesPainted: 0, lastFrameAt: 0,
  };
  function setState(s, err) {
    state.status = s;
    state.error = err || null;
    if (statusEl) statusEl.textContent = "huddlecast chime (" + cfg.mode + "): " + s + (err ? " — " + err : "");
  }

  function waitIce(pc, ms) {
    return new Promise((resolve) => {
      if (pc.iceGatheringState === "complete") return resolve();
      const t = setTimeout(resolve, ms);
      pc.addEventListener("icegatheringstatechange", () => { if (pc.iceGatheringState === "complete") { clearTimeout(t); resolve(); } });
    });
  }

  async function whepPull(url) {
    const pc = new RTCPeerConnection({ iceServers: [] });
    pc.addTransceiver("video", { direction: "recvonly" });
    pc.addTransceiver("audio", { direction: "recvonly" });
    const stream = new MediaStream();
    pc.ontrack = (e) => { stream.addTrack(e.track); };
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    await waitIce(pc, 2000);
    const res = await fetch(url, { method: "POST", headers: { "Content-Type": "application/sdp" }, body: pc.localDescription.sdp });
    if (!res.ok) throw new Error("WHEP " + res.status);
    await pc.setRemoteDescription({ type: "answer", sdp: await res.text() });
    state.whepConnects++;
    return { pc, stream };
  }

  async function chimeSession() {
    const S = window.ChimeSDK;
    if (!S) throw new Error("chime sdk not loaded");
    if (!cfg.meeting || !cfg.attendee) throw new Error("no meeting/attendee config");
    const logger = new S.ConsoleLogger("huddlecast", cfg.debug ? S.LogLevel.INFO : S.LogLevel.WARN);
    const deviceController = new S.DefaultDeviceController(logger);
    const configuration = new S.MeetingSessionConfiguration(cfg.meeting, cfg.attendee);
    const session = new S.DefaultMeetingSession(configuration, logger, deviceController);
    return session.audioVideo;
  }

  // ---- send mode: pull OBS over WHEP; audio → the bot's mic (everyone hears it),
  //      video → the huddle content share ----
  function applyAudioProfile(av) {
    const S = window.ChimeSDK;
    if (!S.AudioProfile || !av.setAudioProfile) return;
    const bps = (cfg.audioMaxKbps || 128) * 1000;
    let profile;
    try {
      if (cfg.audioProfile === "speech") profile = S.AudioProfile.fullbandSpeechMono(bps);
      else if (cfg.audioProfile === "music-mono") profile = S.AudioProfile.fullbandMusicMono(bps);
      else profile = S.AudioProfile.fullbandMusicStereo(bps);
      av.setAudioProfile(profile);
      state.audioProfile = cfg.audioProfile || "music-stereo";
      log("audio profile", state.audioProfile, bps);
    } catch (e) { log("setAudioProfile failed", e); }
  }

  async function runSend() {
    if (!cfg.whepUrl) throw new Error("no whepUrl");
    const av = await chimeSession();
    applyAudioProfile(av);

    // One content-share canvas for the whole session: it draws the live source when
    // up, else the standby image. Content-sharing a *local canvas* track (not the raw
    // remote WHEP track) is what Chime reliably encodes — same reason audio is laundered.
    const canvas = document.createElement("canvas");
    canvas.width = cfg.width; canvas.height = cfg.height;
    const ctx = canvas.getContext("2d", { alpha: false });
    const contentStream = canvas.captureStream(cfg.fps);
    try { contentStream.getVideoTracks()[0].contentHint = "motion"; } catch (_) {}
    const srcVideo = document.createElement("video");
    srcVideo.muted = true; srcVideo.autoplay = true; srcVideo.playsInline = true; document.body.appendChild(srcVideo);
    const standbyImg = new Image();
    standbyImg.src = cfg.standbyUrl || "standby.jpg";
    let haveSource = false;
    let paintCount = 0;
    function draw() {
      const W = canvas.width, H = canvas.height;
      ctx.fillStyle = "#000"; ctx.fillRect(0, 0, W, H);
      if (haveSource && srcVideo.readyState >= 2 && srcVideo.videoWidth > 0) {
        const iw = srcVideo.videoWidth, ih = srcVideo.videoHeight, s = Math.min(W / iw, H / ih);
        const dw = iw * s, dh = ih * s;
        ctx.drawImage(srcVideo, (W - dw) / 2, (H - dh) / 2, dw, dh);
        state.framesPainted++; state.standby = false;
      } else if (standbyImg.complete && standbyImg.naturalWidth) {
        const iw = standbyImg.naturalWidth, ih = standbyImg.naturalHeight, s = Math.max(W / iw, H / ih);
        const dw = iw * s, dh = ih * s;
        ctx.drawImage(standbyImg, (W - dw) / 2, (H - dh) / 2, dw, dh);
        state.standby = true;
      }
      paintCount++;
    }
    // draw on decoded source frames, but throttled to the target fps so we don't
    // repaint/re-encode a 60fps source when only 30 is needed (big CPU save)
    const minGap = 1000 / cfg.fps - 2;
    let lastDraw = 0;
    if (srcVideo.requestVideoFrameCallback) {
      const onFrame = (now) => {
        if (haveSource && now - lastDraw >= minGap) { lastDraw = now; draw(); }
        srcVideo.requestVideoFrameCallback(onFrame);
      };
      srcVideo.requestVideoFrameCallback(onFrame);
      setInterval(() => { if (!haveSource) draw(); }, 200); // standby ticks
    } else {
      setInterval(draw, 1000 / cfg.fps);
    }
    setInterval(() => { state.paintFps = paintCount; paintCount = 0; }, 1000);

    // audio: re-originate the OBS audio through WebAudio (Chime sends silence for a
    // raw remote track), pumped through a muted <audio> so Chrome produces samples.
    const sendCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 48000 });
    const sendDest = sendCtx.createMediaStreamDestination();
    const pump = document.createElement("audio");
    pump.autoplay = true; pump.muted = true; document.body.appendChild(pump);
    let micNode = null;
    function micFrom(track) {
      if (micNode) { try { micNode.disconnect(); } catch (_) {} }
      pump.srcObject = new MediaStream([track]);
      pump.play().catch((e) => log("pump play failed", e));
      micNode = sendCtx.createMediaStreamSource(new MediaStream([track]));
      micNode.connect(sendDest);
      sendCtx.resume().catch(() => {});
      return sendDest.stream.getAudioTracks()[0];
    }

    async function pullWithRetry() {
      for (let attempt = 0; ; attempt++) {
        try { return await whepPull(cfg.whepUrl); }
        catch (e) {
          setState("joined", "waiting for source: " + String((e && e.message) || e));
          await new Promise((r) => setTimeout(r, Math.min(1000 * (attempt + 1), 5000)));
        }
      }
    }

    let current = null;
    let wiring = false;
    let dropped = true;
    let lastBytes = -1, stalls = 0;

    async function wire() {
      if (wiring) return;
      wiring = true;
      try {
        const src = await pullWithRetry();
        current = src;
        dropped = false;
        lastBytes = -1; stalls = 0;
        srcVideo.srcObject = src.stream;
        srcVideo.play().catch(() => {});
        haveSource = true;
        const audio = src.stream.getAudioTracks();
        if (audio.length) {
          try { await av.startAudioInput(new MediaStream([micFrom(audio[0])])); av.realtimeUnmuteLocalAudio(); state.audio = true; }
          catch (e) { log("audio input failed", e); }
        }
        src.pc.addEventListener("connectionstatechange", () => {
          state.pcState = src.pc.connectionState;
          if (["failed", "closed"].includes(src.pc.connectionState)) onDrop("pc:" + src.pc.connectionState);
        });
        state.tracks = src.stream.getVideoTracks().length + "v" + src.stream.getAudioTracks().length + "a";
        setState("live");
      } finally {
        wiring = false;
      }
    }

    async function onDrop(reason) {
      if (dropped) return;
      dropped = true;
      state.dropReason = reason || "";
      state.audio = false;
      haveSource = false; // draw loop switches to the standby image
      try { av.realtimeMuteLocalAudio(); } catch (_) {}
      try { srcVideo.srcObject = null; } catch (_) {}
      if (current) { try { current.pc.close(); } catch (_) {} current = null; }
      setState("joined", "source offline — showing standby");
      wire();
    }

    // watchdog: dead pc, or inbound video bytes that stop growing (OBS stopped)
    setInterval(async () => {
      if (dropped || !current) return;
      state.pcState = current.pc.connectionState;
      if (["failed", "closed"].includes(current.pc.connectionState)) { onDrop("pc-" + current.pc.connectionState); return; }
      try {
        const stats = await current.pc.getStats();
        let bytes = 0, v = null;
        stats.forEach((r) => { if (r.type === "inbound-rtp" && (r.kind === "video" || r.mediaType === "video")) { bytes += r.bytesReceived || 0; v = r; } });
        if (v) {
          state.srcFps = Math.round(v.framesPerSecond || 0);
          if (v.frameWidth) { state.srcRes = v.frameWidth + "x" + v.frameHeight; state.sourceVideo = state.srcRes; }
          const dropped = v.framesDropped || 0;
          if (state._lastDropped != null) state.dropRate = Math.max(0, Math.round((dropped - state._lastDropped) / 4));
          state._lastDropped = dropped;
          state.framesDropped = dropped;
        }
        if (lastBytes >= 0) state.srcKbps = Math.round((bytes - lastBytes) * 8 / 1000 / 4);
        if (lastBytes >= 0 && bytes === lastBytes) { if (++stalls >= 2) onDrop("stalled"); } else stalls = 0;
        lastBytes = bytes;
      } catch (_) {}
    }, 4000);

    const myId = cfg.attendee && (cfg.attendee.AttendeeId || cfg.attendee.attendeeId);
    if (myId) {
      try { av.realtimeSubscribeToVolumeIndicator(myId, (id, vol, muted) => { if (vol != null) state.micLevel = Math.round(vol * 100); state.micMuted = muted; }); }
      catch (e) { log("volume indicator subscribe failed", e); }
    }
    av.addObserver({
      audioVideoDidStart: async () => {
        state.joined = true; setState("joined");
        try { await av.startContentShare(contentStream); state.contentShare = true; } catch (e) { log("content share failed", e); }
        wire().catch((e) => setState("error", String((e && e.message) || e)));
      },
      audioVideoDidStop: (s) => { state.joined = false; state.contentShare = false; state.audio = false; setState("error", "chime stopped: " + (s ? s.statusCode() : "")); },
    });
    setState("joining");
    av.start();
  }

  // ---- receive mode: subscribe to the whitelisted attendee's tile, WHIP it to mediamtx ----
  async function runReceive() {
    const canvas = document.createElement("canvas");
    canvas.width = cfg.width; canvas.height = cfg.height;
    const ctx2d = canvas.getContext("2d", { alpha: false });
    const outVideo = canvas.captureStream(cfg.fps).getVideoTracks()[0];
    try { outVideo.contentHint = cfg.kind === "camera" ? "motion" : "detail"; } catch (_) {}
    const audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 48000 });
    const dest = audioCtx.createMediaStreamDestination();
    { const osc = audioCtx.createOscillator(); const z = audioCtx.createGain(); z.gain.value = 0; osc.connect(z).connect(dest); osc.start(); }
    const outAudio = dest.stream.getAudioTracks()[0];
    const audioEl = document.createElement("audio"); audioEl.autoplay = true; audioEl.muted = true; document.body.appendChild(audioEl);
    const tileVideo = document.createElement("video"); tileVideo.autoplay = true; tileVideo.muted = true; tileVideo.playsInline = true; tileVideo.style.display = "none"; document.body.appendChild(tileVideo);
    let audioWired = false;

    const liveVideo = (v) => v.readyState >= 2 && v.videoWidth > 0;
    setInterval(() => {
      const W = canvas.width, H = canvas.height;
      if (liveVideo(tileVideo)) {
        const vw = tileVideo.videoWidth, vh = tileVideo.videoHeight, scale = Math.min(W / vw, H / vh);
        const dw = Math.round(vw * scale), dh = Math.round(vh * scale);
        ctx2d.fillStyle = "#000"; ctx2d.fillRect(0, 0, W, H);
        ctx2d.drawImage(tileVideo, (W - dw) / 2, (H - dh) / 2, dw, dh);
        state.sourceVideo = vw + "x" + vh; state.framesPainted++; state.lastFrameAt = Date.now();
      } else {
        ctx2d.fillStyle = "#111"; ctx2d.fillRect(0, 0, W, H);
        ctx2d.fillStyle = "#888"; ctx2d.font = "26px sans-serif"; ctx2d.textAlign = "center";
        ctx2d.fillText("huddlecast: waiting for the whitelisted participant to share", W / 2, H / 2);
      }
    }, 1000 / cfg.fps);

    let pc = null, reconnectTimer = null, backoff = 1000;
    const scheduleReconnect = () => { if (reconnectTimer) return; reconnectTimer = setTimeout(() => { reconnectTimer = null; publish(); }, backoff); backoff = Math.min(backoff * 2, 15000); };
    async function publish() {
      if (!cfg.whipUrl) { setState("error", "no whipUrl"); return; }
      setState(state.publishes ? "reconnecting" : "connecting");
      if (pc) { try { pc.close(); } catch (_) {} pc = null; }
      pc = new RTCPeerConnection({ iceServers: [] });
      const ms = new MediaStream([outVideo, outAudio]);
      pc.addTransceiver(outVideo, { direction: "sendonly", streams: [ms] });
      pc.addTransceiver(outAudio, { direction: "sendonly", streams: [ms] });
      pc.onconnectionstatechange = () => { if (["failed", "disconnected", "closed"].includes(pc.connectionState)) scheduleReconnect(); };
      try {
        const offer = await pc.createOffer(); await pc.setLocalDescription(offer); await waitIce(pc, 2000);
        const headers = { "Content-Type": "application/sdp" }; if (cfg.token) headers.Authorization = "Bearer " + cfg.token;
        const res = await fetch(cfg.whipUrl, { method: "POST", headers, body: pc.localDescription.sdp });
        if (!res.ok) throw new Error("WHIP " + res.status);
        await pc.setRemoteDescription({ type: "answer", sdp: await res.text() });
        state.publishes++; backoff = 1000; setState("live");
      } catch (err) { setState("error", String((err && err.message) || err)); scheduleReconnect(); }
    }

    const av = await chimeSession();
    state.targets = (cfg.targetUserIDs || []).join(",");
    const seen = {};
    let boundTileId = null;
    const userMatches = (id) => { if (!id) return false; const base = id.split("#")[0]; return cfg.targetUserIDs.some((u) => base === u || base.endsWith("-" + u) || base.endsWith(u)); };
    const chooseTile = (t) => { if (t.localTile || !userMatches(t.boundExternalUserId)) return false; if (cfg.kind === "screen" && !t.isContent) return false; if (cfg.kind === "camera" && t.isContent) return false; return true; };
    av.addObserver({
      audioVideoDidStart: () => { state.joined = true; setState("joined"); publish(); },
      audioVideoDidStop: (s) => setState("error", "chime stopped: " + (s ? s.statusCode() : "")),
      videoTileDidUpdate: (t) => {
        if (!t.tileId) return;
        seen[t.tileId] = (t.boundExternalUserId || "?") + (t.isContent ? "#content" : "") + (t.localTile ? "(self)" : "");
        state.seen = Object.values(seen).join(" | ");
        state.tiles = av.getAllVideoTiles().length;
        if (boundTileId !== null && t.tileId !== boundTileId) {
          if (cfg.kind === "" && t.isContent && userMatches(t.boundExternalUserId)) { /* prefer screen */ } else return;
        }
        if (boundTileId === null && !chooseTile(t)) return;
        boundTileId = t.tileId; state.targetUser = t.boundExternalUserId || ""; state.targetKind = t.isContent ? "screen" : "camera";
        av.bindVideoElement(t.tileId, tileVideo);
      },
      videoTileWasRemoved: (id) => { if (id === boundTileId) { boundTileId = null; state.sourceVideo = ""; } },
    });
    av.bindAudioElement(audioEl);
    if (!audioWired) { try { const s = audioEl.captureStream ? audioEl.captureStream() : audioEl.mozCaptureStream(); audioCtx.createMediaStreamSource(s).connect(dest); audioWired = true; } catch (e) { log("audio wire failed", e); } }
    setState("joining");
    av.start();
    audioCtx.resume().catch(() => {});
  }

  window.__huddlecastChime = {
    cfg, state,
    snapshot() { return JSON.parse(JSON.stringify(Object.assign({}, state, { now: Date.now() }))); },
  };
  const run = cfg.mode === "send" ? runSend : runReceive;
  run().catch((e) => setState("error", String((e && e.message) || e)));
})();
