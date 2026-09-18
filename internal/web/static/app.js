const HC_PRESETS = {
  "low": { label: "Low bandwidth", profile: "speech", audio_kbps: 64, video_kbps: 1000, fps: 24, width: 854, height: 480 },
  "balanced": { label: "Balanced", profile: "music-stereo", audio_kbps: 128, video_kbps: 2500, fps: 30, width: 1280, height: 720 },
  "high-audio": { label: "High-quality audio", profile: "music-stereo", audio_kbps: 256, video_kbps: 2500, fps: 30, width: 1280, height: 720 },
  "high-video": { label: "High-quality video", profile: "music-stereo", audio_kbps: 160, video_kbps: 6000, fps: 30, width: 1920, height: 1080 },
  "max": { label: "Max (1080p60)", profile: "music-stereo", audio_kbps: 320, video_kbps: 8000, fps: 60, width: 1920, height: 1080 },
};
function hcPreset(sel) {
  const p = HC_PRESETS[sel.value];
  if (!p) return;
  const form = sel.closest("form") || document;
  const set = (n, v) => { const el = form.querySelector("[name=" + n + "]"); if (el) el.value = v; };
  set("q_profile", p.profile); set("q_audio_kbps", p.audio_kbps); set("q_video_kbps", p.video_kbps);
  set("q_fps", p.fps); set("q_width", p.width); set("q_height", p.height);
}
document.addEventListener("DOMContentLoaded", () => {
  document.querySelectorAll("select[data-preset]").forEach((sel) => {
    for (const key of Object.keys(HC_PRESETS)) {
      const o = document.createElement("option");
      o.value = key; o.textContent = HC_PRESETS[key].label;
      sel.appendChild(o);
    }
    sel.addEventListener("change", () => hcPreset(sel));
  });
  document.querySelectorAll("video[data-whep]").forEach((v) => hcPreview(v, v.dataset.whep));
});

function hcPreview(video, url) {
  let pc = null, timer = null, stopped = false;
  async function connect() {
    try {
      pc = new RTCPeerConnection({ iceServers: [] });
      pc.addTransceiver("video", { direction: "recvonly" });
      pc.addTransceiver("audio", { direction: "recvonly" });
      const ms = new MediaStream();
      pc.ontrack = (e) => { ms.addTrack(e.track); video.srcObject = ms; video.play().catch(() => {}); };
      pc.onconnectionstatechange = () => { if (["failed", "disconnected", "closed"].includes(pc.connectionState)) retry(); };
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await new Promise((r) => { if (pc.iceGatheringState === "complete") return r(); const t = setTimeout(r, 2000); pc.addEventListener("icegatheringstatechange", () => { if (pc.iceGatheringState === "complete") { clearTimeout(t); r(); } }); });
      const res = await fetch(url, { method: "POST", headers: { "Content-Type": "application/sdp" }, body: pc.localDescription.sdp });
      if (!res.ok) throw new Error("WHEP " + res.status);
      await pc.setRemoteDescription({ type: "answer", sdp: await res.text() });
    } catch (e) { retry(); }
  }
  function retry() { if (stopped) return; try { pc && pc.close(); } catch (_) {} video.srcObject = null; clearTimeout(timer); timer = setTimeout(connect, 3000); }
  connect();
}

function hcReveal(btn) {
  const el = btn.previousElementSibling;
  const shown = el.dataset.shown === '1';
  el.textContent = shown ? '•'.repeat(Math.min(24, el.dataset.secret.length)) : el.dataset.secret;
  el.dataset.shown = shown ? '0' : '1';
  btn.textContent = shown ? 'Show' : 'Hide';
  const row = btn.closest('.Box-row');
  if (row) row.querySelectorAll('.secret-details').forEach((d) => d.classList.toggle('hidden', shown));
}

function hcSource() {
  const mirror = document.querySelector('input[name=source_type][value=mirror]').checked;
  document.getElementById('src-push').classList.toggle('hidden', mirror);
  document.getElementById('src-mirror').classList.toggle('hidden', !mirror);
}

function hcSession(id, running) {
  const chat = document.getElementById('chat');
  let lastId = 0;
  chat.querySelectorAll('.msg').forEach((m) => { lastId = Math.max(lastId, Number(m.dataset.id)); });
  chat.scrollTop = chat.scrollHeight;

  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
  function appendChat(m) {
    if (m.ID <= lastId) return;
    lastId = m.ID;
    const div = document.createElement('div');
    div.className = 'msg';
    div.dataset.id = m.ID;
    div.innerHTML = `<span class="chan">#${esc(m.ChannelName)}</span> <span class="user">${esc(m.UserName)}</span> <span class="text">${esc(m.Text)}</span>`;
    chat.appendChild(div);
    chat.scrollTop = chat.scrollHeight;
  }
  async function pullChat() {
    try {
      const res = await fetch(`/sessions/${id}/chat.json?after=${lastId}`);
      (await res.json()).forEach(appendChat);
    } catch (_) {}
  }

  if (!running) return;

  const es = new EventSource(`/events?session=${id}`);
  es.addEventListener('chat', (e) => appendChat(JSON.parse(e.data).data));
  es.addEventListener('target', (e) => {
    const d = JSON.parse(e.data).data;
    const card = document.querySelector(`[data-target="${d.targetId}"]`);
    if (!card) return location.reload();
    const pill = card.querySelector('[data-status]');
    pill.textContent = d.status;
    pill.className = 'pill st-' + d.status;
    card.querySelector('[data-error]').textContent = d.error || '';
  });
  es.addEventListener('session', () => location.reload());
  es.addEventListener('mirror', () => {});
  es.onerror = () => setTimeout(pullChat, 3000);

  async function pullStats() {
    try {
      const rows = await (await fetch(`/sessions/${id}/stats.json`)).json();
      rows.forEach((d) => {
        const card = document.querySelector(`[data-target="${d.targetId}"]`);
        if (!card) return;
        const pill = card.querySelector('[data-status]');
        if (pill) { pill.textContent = d.status; pill.className = 'pill st-' + d.status; }
        const err = card.querySelector('[data-error]');
        if (err) err.textContent = d.error || '';
        const st = card.querySelector('[data-stats]');
        if (st) {
          const tf = d.targetFps || 30;
          const ratio = tf ? d.paintFps / tf : 1;
          let health = 'smooth', cls = 'ok';
          if (d.standby) { health = 'standby'; cls = 'warn'; }
          else if (d.status !== 'streaming') { health = d.status; cls = 'warn'; }
          else if (ratio < 0.6 || d.dropRate >= 5) { health = 'struggling'; cls = 'bad'; }
          else if (ratio < 0.9 || d.dropRate >= 1) { health = 'choppy'; cls = 'warn'; }
          st.innerHTML = `<span class="pill ${cls}">${health}</span> <span class="color-fg-muted"> ${d.res || '—'} · in ${d.srcFps}fps ${d.srcKbps}kbps · out ${d.paintFps}/${tf}fps · dropped ${d.dropRate}/s</span>`;
        }
      });
    } catch (_) {}
  }
  setInterval(pullStats, 2000);
  setInterval(pullChat, 15000);
}
