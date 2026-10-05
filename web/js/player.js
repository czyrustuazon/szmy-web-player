// Playback engine.
//
// Two paths share one Web Audio graph (source -> analyser -> volume -> out):
//   element: a normal <audio> element streaming from the server (Range requests).
//   buffer:  for tracks with loop points. The file is decoded fully and played
//            by an AudioBufferSourceNode with loopStart/loopEnd, which loops
//            sample-accurately and gaplessly.

import { api } from './api.js';

// Position inside a looping track after `elapsed` seconds of playback.
export function loopPosition(elapsed, ls, le) {
  if (elapsed < le) return elapsed;
  return ls + ((elapsed - ls) % (le - ls));
}

// Seconds of playback left until the loop section has played `count` times.
export function loopRunTime(offset, ls, le, count) {
  return le - offset + (Math.max(1, count) - 1) * (le - ls);
}

function canPlayOpus() {
  const r = document.createElement('audio').canPlayType('audio/ogg; codecs="opus"');
  return r === 'maybe' || r === 'probably';
}

function mediaErrorText(a) {
  switch (a.error && a.error.code) {
    case 2:
      return 'network error';
    case 3:
      return 'could not decode this audio';
    case 4:
      return 'format not supported';
    default:
      return 'playback error';
  }
}

export class Player extends EventTarget {
  constructor() {
    super();
    this.audio = new Audio();
    this.audio.preload = 'auto';
    this.ctx = null;
    this.analyser = null;
    this.volGain = null;
    this.volume = 1;
    this.loopCfg = { mode: 'count', count: 2, fade: 10 };

    this.mode = 'idle'; // idle | element | buffer
    this.track = null;
    this.meta = null;
    this.playing = false;
    this.loading = false;

    this._token = 0;
    this._buf = null;
    this._src = null;
    this._t0 = 0;
    this._ls = 0;
    this._le = 0;
    this._pendingOffset = 0;
    this._tick = 0;
    this._wireElement();
  }

  emit(name, detail) {
    this.dispatchEvent(new CustomEvent(name, { detail }));
  }

  _emitState() {
    this.emit('state', { playing: this.playing, loading: this.loading, mode: this.mode });
  }

  _wireElement() {
    const a = this.audio;
    a.addEventListener('timeupdate', () => this.mode === 'element' && this.emit('time'));
    a.addEventListener('ended', () => {
      if (this.mode !== 'element') return;
      this.playing = false;
      this._emitState();
      this.emit('ended');
    });
    a.addEventListener('play', () => {
      if (this.mode !== 'element') return;
      this.playing = true;
      this._emitState();
    });
    a.addEventListener('pause', () => {
      // Lock-screen / headset pause arrives here too.
      if (this.mode !== 'element' || a.ended) return;
      this.playing = false;
      this._emitState();
    });
    a.addEventListener('waiting', () => {
      this.loading = true;
      this._emitState();
    });
    a.addEventListener('playing', () => {
      this.loading = false;
      this._emitState();
    });
    a.addEventListener('error', () => {
      if (this.loading || !a.getAttribute('src')) return; // load() reports its own failures
      this.playing = false;
      this._emitState();
      this.emit('error', { message: mediaErrorText(a) });
    });
  }

  ensureContext() {
    if (this.ctx) return this.ctx;
    const Ctx = window.AudioContext || window.webkitAudioContext;
    this.ctx = new Ctx();
    this.analyser = this.ctx.createAnalyser();
    this.analyser.fftSize = 2048;
    this.analyser.smoothingTimeConstant = 0.6;
    this.volGain = this.ctx.createGain();
    this.volGain.gain.value = this.volume;
    this.ctx.createMediaElementSource(this.audio).connect(this.analyser);
    this.analyser.connect(this.volGain);
    this.volGain.connect(this.ctx.destination);
    return this.ctx;
  }

  setVolume(v) {
    this.volume = Math.min(1, Math.max(0, v));
    if (this.volGain) this.volGain.gain.setTargetAtTime(this.volume, this.ctx.currentTime, 0.02);
  }

  setLoopConfig(cfg) {
    this.loopCfg = { ...this.loopCfg, ...cfg }; // applies from the next track
  }

  get position() {
    if (this.mode === 'element') return this.audio.currentTime || 0;
    if (this.mode === 'buffer') {
      if (!this._src) return this._pendingOffset;
      return loopPosition(this.ctx.currentTime - this._t0, this._ls, this._le);
    }
    return 0;
  }

  get duration() {
    if (this.mode === 'element') return Number.isFinite(this.audio.duration) ? this.audio.duration : 0;
    if (this.mode === 'buffer' && this._buf) return this._buf.duration;
    return 0;
  }

  // True while a looping track will keep going until the user moves on.
  get loopsForever() {
    return this.mode === 'buffer' && this.loopCfg.mode === 'forever';
  }

  _teardown() {
    this.mode = 'idle';
    this._stopTick();
    if (this._src) {
      const old = this._src;
      this._src = null;
      try {
        old.stop();
      } catch {
        /* already stopped */
      }
    }
    this._buf = null;
    this.audio.pause();
    this.audio.removeAttribute('src');
    this.audio.load();
  }

  clear() {
    this._token++;
    this._teardown();
    this.track = null;
    this.meta = null;
    this.playing = false;
    this.loading = false;
    this._emitState();
  }

  // Loads a track. Resolves true when ready (and playing if autoplay).
  async load(track, meta, { autoplay = true, position = 0 } = {}) {
    const token = ++this._token;
    this._teardown();
    this.track = track;
    this.meta = meta;
    this.playing = false;
    this.loading = true;
    this._pendingOffset = 0;
    this._emitState();
    this.ensureContext();
    try {
      if (meta.loop && this.loopCfg.mode !== 'once') await this._loadBuffer(track, meta, position, token);
      else await this._loadElement(track, meta, position, token);
    } catch (err) {
      if (token !== this._token) return false;
      this.loading = false;
      this._emitState();
      this.emit('error', { message: err.message || 'could not load track' });
      return false;
    }
    if (token !== this._token) return false;
    this.loading = false;
    if (autoplay) await this.play();
    else this._emitState();
    return true;
  }

  async _loadElement(track, meta, position, token) {
    this.mode = 'element';
    const a = this.audio;
    a.src = api.streamURL(track.path, meta.kind === 'opus' && !canPlayOpus());
    await new Promise((resolve, reject) => {
      const cleanup = () => {
        a.removeEventListener('loadedmetadata', ok);
        a.removeEventListener('error', bad);
      };
      const ok = () => (cleanup(), resolve());
      const bad = () => (cleanup(), reject(new Error(mediaErrorText(a))));
      a.addEventListener('loadedmetadata', ok);
      a.addEventListener('error', bad);
    });
    if (token === this._token && position > 0) a.currentTime = Math.min(position, a.duration || position);
  }

  async _loadBuffer(track, meta, position, token) {
    this.mode = 'buffer';
    const res = await fetch(api.streamURL(track.path), { credentials: 'same-origin' });
    if (!res.ok) throw new Error(`could not fetch audio (HTTP ${res.status})`);
    const buf = await this.ctx.decodeAudioData(await res.arrayBuffer());
    if (token !== this._token) return;
    this._buf = buf;
    this._pendingOffset = Math.min(Math.max(0, position), Math.max(0, buf.duration - 0.05));
  }

  async play() {
    if (!this.track || this.mode === 'idle') return;
    try {
      await this.ctx.resume();
      if (this.mode === 'element') await this.audio.play();
      else if (!this._src) this._startBuffer(this._pendingOffset);
    } catch (err) {
      this.playing = false;
      this._emitState();
      this.emit('error', { message: err.name === 'NotAllowedError' ? 'tap play to start audio' : err.message });
      return;
    }
    this.playing = true;
    this._startTick();
    this._emitState();
  }

  pause() {
    if (this.mode === 'element') this.audio.pause();
    else if (this.mode === 'buffer' && this._src) this.ctx.suspend();
    this.playing = false;
    this._stopTick();
    this._emitState();
  }

  toggle() {
    return this.playing ? this.pause() : this.play();
  }

  seek(t) {
    if (!this.track) return;
    if (this.mode === 'element') {
      this.audio.currentTime = Math.max(0, t);
    } else if (this.mode === 'buffer') {
      const offset = Math.min(Math.max(0, t), Math.max(0, this._buf.duration - 0.05));
      this._pendingOffset = offset;
      if (this._src) {
        const old = this._src;
        this._src = null; // so its onended is ignored
        try {
          old.stop();
        } catch {
          /* already stopped */
        }
        this._startBuffer(offset); // ctx stays suspended if we were paused
      }
    }
    this.emit('time');
  }

  _startBuffer(offset) {
    const loop = this.meta.loop;
    const buf = this._buf;
    const ls = loop.start / loop.sampleRate;
    const le = Math.min(loop.end / loop.sampleRate, buf.duration);
    const start = Math.min(Math.max(0, offset), Math.max(0, le - 0.001));

    const src = this.ctx.createBufferSource();
    src.buffer = buf;
    src.loop = true;
    src.loopStart = ls;
    src.loopEnd = le;
    const gain = this.ctx.createGain();
    src.connect(gain);
    gain.connect(this.analyser);

    const now = this.ctx.currentTime;
    this._t0 = now - start;
    this._ls = ls;
    this._le = le;

    if (this.loopCfg.mode === 'count') {
      // Play the loop `count` times, then fade out and finish (vgmstream's default behaviour).
      const endAt = now + loopRunTime(start, ls, le, this.loopCfg.count);
      const fade = Math.max(0, this.loopCfg.fade);
      if (fade > 0) {
        gain.gain.setValueAtTime(1, endAt);
        gain.gain.linearRampToValueAtTime(0, endAt + fade);
      }
      src.stop(endAt + fade);
    }

    src.onended = () => {
      if (this._src !== src) return;
      this._src = null;
      this._pendingOffset = 0;
      this.playing = false;
      this._stopTick();
      this._emitState();
      this.emit('ended');
    };
    this._src = src;
    src.start(0, start);
  }

  _startTick() {
    this._stopTick();
    if (this.mode === 'buffer') this._tick = setInterval(() => this.emit('time'), 250);
  }

  _stopTick() {
    clearInterval(this._tick);
    this._tick = 0;
  }
}
