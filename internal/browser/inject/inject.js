(() => {
  if (window.__huddlecast) return;
  const cfg = Object.assign(
    { whepUrl: "", presentation: "screen", width: 1280, height: 720, fps: 30, debug: false },
    window.__HUDDLECAST_CFG || {}
  );
  const log = (...a) => cfg.debug && console.log("[huddlecast]", ...a);

  const state = {
    status: "init",
    error: null,
    whepConnects: 0,
    lastFrameAt: 0,
    framesPainted: 0,
    remoteTracks: { audio: 0, video: 0 },
    handedOut: { getUserMedia: 0, getDisplayMedia: 0 },
    presentation: cfg.presentation,
  };

  const canvas = document.createElement("canvas");
  canvas.width = cfg.width;
  canvas.height = cfg.height;
  const ctx2d = canvas.getContext("2d", { alpha: false });
  ctx2d.fillStyle = "#000";
  ctx2d.fillRect(0, 0, canvas.width, canvas.height);
  const canvasStream = canvas.captureStream(cfg.fps);
  const stableVideo = canvasStream.getVideoTracks()[0];
  try { stableVideo.contentHint = "motion"; } catch (_) {}

  const audioCtx = new (window.AudioContext || window.webkitAudioContext)({ sampleRate: 48000 });
  const audioDest = audioCtx.createMediaStreamDestination();
  {
    const osc = audioCtx.createOscillator();
    const zero = audioCtx.createGain();
    zero.gain.value = 0;
    osc.connect(zero).connect(audioDest);
    osc.start();
    audioCtx.resume().catch(() => {});
  }
  const stableAudio = audioDest.stream.getAudioTracks()[0];
  let audioSourceNode = null;

  const video = document.createElement("video");
  video.__huddlecastOwn = true;
  video.muted = true;
  video.autoplay = true;
  video.playsInline = true;
  video.style.cssText = "position:fixed;left:-9999px;top:-9999px;width:16px;height:16px;opacity:0;pointer-events:none";
  let pc = null;
  let reconnectTimer = null;
  let backoff = 1000;

  function attachDom() {
    if (!video.isConnected && document.documentElement) {
      (document.body || document.documentElement).appendChild(video);
    }
  }

  function waitIce(pc, timeoutMs) {
    return new Promise((resolve) => {
      if (pc.iceGatheringState === "complete") return resolve();
      const t = setTimeout(resolve, timeoutMs);
      pc.addEventListener("icegatheringstatechange", () => {
        if (pc.iceGatheringState === "complete") { clearTimeout(t); resolve(); }
      });
    });
  }

  function scheduleReconnect() {
    if (reconnectTimer) return;
    reconnectTimer = setTimeout(() => {
      reconnectTimer = null;
      connectWHEP();
    }, backoff);
    backoff = Math.min(backoff * 2, 15000);
  }

  async function connectWHEP() {
    if (!cfg.whepUrl) { state.status = "error"; state.error = "no whepUrl"; return; }
    state.status = state.whepConnects ? "reconnecting" : "connecting";
    if (pc) { try { pc.close(); } catch (_) {} pc = null; }
    const remote = new MediaStream();
    pc = new RTCPeerConnection({ iceServers: [] });
    pc.addTransceiver("video", { direction: "recvonly" });
    pc.addTransceiver("audio", { direction: "recvonly" });
    pc.ontrack = (e) => {
      remote.addTrack(e.track);
      state.remoteTracks[e.track.kind]++;
      log("track", e.track.kind);
    };
    pc.onconnectionstatechange = () => {
      log("pc state", pc.connectionState);
      if (["failed", "disconnected", "closed"].includes(pc.connectionState)) scheduleReconnect();
    };
    try {
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await waitIce(pc, 2000);
      const res = await fetch(cfg.whepUrl, {
        method: "POST",
        headers: { "Content-Type": "application/sdp" },
        body: pc.localDescription.sdp,
      });
      if (!res.ok) throw new Error("WHEP " + res.status + " (is anyone publishing?)");
      const answer = await res.text();
      await pc.setRemoteDescription({ type: "answer", sdp: answer });
      attachDom();
      video.srcObject = remote;
      await video.play().catch(() => {});
      if (audioSourceNode) { try { audioSourceNode.disconnect(); } catch (_) {} }
      if (remote.getAudioTracks().length) {
        audioSourceNode = audioCtx.createMediaStreamSource(new MediaStream(remote.getAudioTracks()));
        audioSourceNode.connect(audioDest);
      }
      audioCtx.resume().catch(() => {});
      state.status = "live";
      state.error = null;
      state.whepConnects++;
      backoff = 1000;
      log("WHEP live");
    } catch (err) {
      state.status = "error";
      state.error = String((err && err.message) || err);
      log("WHEP failed:", state.error);
      scheduleReconnect();
    }
  }

  function paint() {
    if (video.readyState >= 2 && !video.paused && !video.ended) {
      ctx2d.drawImage(video, 0, 0, canvas.width, canvas.height);
      state.framesPainted++;
      state.lastFrameAt = Date.now();
    }
  }
  function rvfcLoop() {
    if (typeof video.requestVideoFrameCallback === "function") {
      video.requestVideoFrameCallback(() => { paint(); rvfcLoop(); });
    }
  }
  rvfcLoop();
  setInterval(() => {
    if (Date.now() - state.lastFrameAt > (1000 / cfg.fps) * 2) paint();
    if (state.status === "live" && state.lastFrameAt && Date.now() - state.lastFrameAt > 10000) {
      log("frame watchdog fired");
      state.status = "reconnecting";
      scheduleReconnect();
    }
  }, 1000 / cfg.fps);

  const md = navigator.mediaDevices;
  const origGUM = md.getUserMedia.bind(md);
  const origGDM = md.getDisplayMedia ? md.getDisplayMedia.bind(md) : null;
  const wantsVideo = (c) => !!(c && c.video);
  const wantsAudio = (c) => !!(c && c.audio);

  function silentAudioTrack() {
    const g = audioCtx.createGain();
    g.gain.value = 0;
    const dest = audioCtx.createMediaStreamDestination();
    g.connect(dest);
    return dest.stream.getAudioTracks()[0];
  }
  function blackVideoTrack() {
    const c = document.createElement("canvas");
    c.width = 320; c.height = 180;
    c.getContext("2d").fillRect(0, 0, 320, 180);
    return c.captureStream(1).getVideoTracks()[0];
  }

  md.getUserMedia = async (constraints) => {
    state.handedOut.getUserMedia++;
    const out = new MediaStream();
    const p = state.presentation;
    if (wantsAudio(constraints)) out.addTrack(p === "none" ? silentAudioTrack() : stableAudio.clone());
    if (wantsVideo(constraints)) out.addTrack(p === "camera" || p === "both" ? stableVideo.clone() : blackVideoTrack());
    log("getUserMedia ->", out.getTracks().map((t) => t.kind).join(","));
    return out;
  };

  md.getDisplayMedia = async (constraints) => {
    state.handedOut.getDisplayMedia++;
    if (state.presentation === "none") {
      if (origGDM) return origGDM(constraints);
      throw new DOMException("NotAllowedError", "NotAllowedError");
    }
    const out = new MediaStream([stableVideo.clone()]);
    if (wantsAudio(constraints)) out.addTrack(stableAudio.clone());
    log("getDisplayMedia ->", out.getTracks().map((t) => t.kind).join(","));
    return out;
  };

  md.__huddlecastOriginal = { getUserMedia: origGUM, getDisplayMedia: origGDM };

  window.__huddlecast = {
    cfg,
    state,
    setPresentation(p) { state.presentation = p; },
    reconnect() { backoff = 1000; scheduleReconnect(); },
    snapshot() {
      return JSON.parse(JSON.stringify(Object.assign({}, state, {
        videoReady: video.readyState,
        audioCtx: audioCtx.state,
        now: Date.now(),
      })));
    },
  };

  if (cfg.presentation !== "none") {
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", connectWHEP, { once: true });
    } else {
      connectWHEP();
    }
  } else {
    state.status = "live";
  }
})();
