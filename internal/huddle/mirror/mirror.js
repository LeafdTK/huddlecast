(() => {
  if (window.__huddlecastMirror) return;
  const cfg = Object.assign(
    { whipUrl: "", token: "", width: 1280, height: 720, fps: 30, debug: false },
    window.__HUDDLECAST_MIRROR_CFG || {}
  );
  const log = (...a) => cfg.debug && console.log("[huddlecast-mirror]", ...a);
  const state = { status: "init", error: null, publishes: 0, sourceVideo: "", audioSources: 0, framesPainted: 0, lastFrameAt: 0 };

  const canvas = document.createElement("canvas");
  canvas.width = cfg.width;
  canvas.height = cfg.height;
  const ctx2d = canvas.getContext("2d", { alpha: false });
  const outVideo = canvas.captureStream(cfg.fps).getVideoTracks()[0];
  try { outVideo.contentHint = "motion"; } catch (_) {}

  const audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 48000 });
  const dest = audioCtx.createMediaStreamDestination();
  {
    const osc = audioCtx.createOscillator();
    const zero = audioCtx.createGain();
    zero.gain.value = 0;
    osc.connect(zero).connect(dest);
    osc.start();
  }
  const outAudio = dest.stream.getAudioTracks()[0];
  const wired = new WeakSet();
  let srcVideo = null;

  function liveVideo(v) {
    const s = v.srcObject;
    return s && typeof s.getVideoTracks === "function" && s.getVideoTracks().some((t) => t.readyState === "live") && v.readyState >= 2 && v.videoWidth > 0;
  }
  function pickVideo() {
    const cands = Array.from(document.querySelectorAll("video")).filter((v) => !v.__huddlecastOwn && liveVideo(v));
    cands.sort((a, b) => (b.videoWidth * b.videoHeight) - (a.videoWidth * a.videoHeight) || (b.clientWidth * b.clientHeight) - (a.clientWidth * a.clientHeight));
    const next = cands[0] || null;
    if (next !== srcVideo) {
      srcVideo = next;
      state.sourceVideo = next ? `${next.videoWidth}x${next.videoHeight}` : "";
      log("source video ->", state.sourceVideo);
    }
  }
  function wireAudio() {
    for (const el of document.querySelectorAll("audio, video")) {
      const s = el.srcObject;
      if (!s || el.__huddlecastOwn || typeof s.getAudioTracks !== "function" || !s.getAudioTracks().length || wired.has(s)) continue;
      try {
        audioCtx.createMediaStreamSource(s).connect(dest);
        wired.add(s);
        state.audioSources++;
        log("wired audio source");
      } catch (e) { log("audio wire failed", e); }
    }
    audioCtx.resume().catch(() => {});
  }
  setInterval(() => { pickVideo(); wireAudio(); }, 1000);

  function paint() {
    const W = canvas.width, H = canvas.height;
    if (srcVideo && liveVideo(srcVideo)) {
      const vw = srcVideo.videoWidth, vh = srcVideo.videoHeight;
      const scale = Math.min(W / vw, H / vh);
      const dw = Math.round(vw * scale), dh = Math.round(vh * scale);
      ctx2d.fillStyle = "#000";
      ctx2d.fillRect(0, 0, W, H);
      ctx2d.drawImage(srcVideo, (W - dw) / 2, (H - dh) / 2, dw, dh);
      state.framesPainted++;
      state.lastFrameAt = Date.now();
    } else {
      ctx2d.fillStyle = "#111";
      ctx2d.fillRect(0, 0, W, H);
      ctx2d.fillStyle = "#888";
      ctx2d.font = "28px sans-serif";
      ctx2d.textAlign = "center";
      ctx2d.fillText("huddlecast: waiting for a screen share in the source huddle", W / 2, H / 2);
    }
  }
  setInterval(paint, 1000 / cfg.fps);

  let pc = null, reconnectTimer = null, backoff = 1000;
  function waitIce(pc, ms) {
    return new Promise((resolve) => {
      if (pc.iceGatheringState === "complete") return resolve();
      const t = setTimeout(resolve, ms);
      pc.addEventListener("icegatheringstatechange", () => { if (pc.iceGatheringState === "complete") { clearTimeout(t); resolve(); } });
    });
  }
  function scheduleReconnect() {
    if (reconnectTimer) return;
    reconnectTimer = setTimeout(() => { reconnectTimer = null; publish(); }, backoff);
    backoff = Math.min(backoff * 2, 15000);
  }
  async function publish() {
    if (!cfg.whipUrl) { state.status = "error"; state.error = "no whipUrl"; return; }
    state.status = state.publishes ? "reconnecting" : "connecting";
    if (pc) { try { pc.close(); } catch (_) {} pc = null; }
    pc = new RTCPeerConnection({ iceServers: [] });
    const ms = new MediaStream([outVideo, outAudio]);
    pc.addTransceiver(outVideo, { direction: "sendonly", streams: [ms] });
    pc.addTransceiver(outAudio, { direction: "sendonly", streams: [ms] });
    pc.onconnectionstatechange = () => {
      log("pc state", pc.connectionState);
      if (["failed", "disconnected", "closed"].includes(pc.connectionState)) scheduleReconnect();
    };
    try {
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await waitIce(pc, 2000);
      const headers = { "Content-Type": "application/sdp" };
      if (cfg.token) headers.Authorization = "Bearer " + cfg.token;
      const res = await fetch(cfg.whipUrl, { method: "POST", headers, body: pc.localDescription.sdp });
      if (!res.ok) throw new Error("WHIP " + res.status);
      await pc.setRemoteDescription({ type: "answer", sdp: await res.text() });
      state.status = "live";
      state.error = null;
      state.publishes++;
      backoff = 1000;
      log("WHIP live");
    } catch (err) {
      state.status = "error";
      state.error = String((err && err.message) || err);
      log("WHIP failed:", state.error);
      scheduleReconnect();
    }
  }

  window.__huddlecastMirror = {
    cfg, state,
    reconnect() { backoff = 1000; scheduleReconnect(); },
    snapshot() { return JSON.parse(JSON.stringify(Object.assign({}, state, { audioCtx: audioCtx.state, now: Date.now() }))); },
  };
  publish();
})();
