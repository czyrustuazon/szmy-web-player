// Visualizer: equalizer bars and waveform scope, like szmy's audio_viz.
//
// Bars: log-spaced bands from 80 Hz to 12 kHz. A bar rises instantly to a
// new peak and falls by "half plus 8" of its value per tick (0..255 scale).
// Scope: peak-preserving decimation of the time-domain signal.

const BANDS = 24;
const TICK_MS = 33;

export function bandEdges(sampleRate, bins, bands = BANDS) {
  const lo = 80;
  const hi = Math.min(12000, sampleRate / 2);
  const edges = [];
  for (let i = 0; i <= bands; i++) {
    const f = lo * Math.pow(hi / lo, i / bands);
    edges.push(Math.min(bins - 1, Math.round((f / (sampleRate / 2)) * (bins - 1))));
  }
  return edges;
}

export function decay(v) {
  return Math.max(0, v - (v / 2 + 8));
}

export class Visualizer {
  constructor(canvas, analyser, sampleRate) {
    this.canvas = canvas;
    this.analyser = analyser;
    this.ctx2d = canvas.getContext('2d');
    this.mode = 'bars';
    this.raf = 0;
    this.last = 0;
    this.bars = new Array(BANDS).fill(0);
    this.freq = new Uint8Array(analyser.frequencyBinCount);
    this.wave = new Uint8Array(analyser.fftSize);
    this.edges = bandEdges(sampleRate, analyser.frequencyBinCount);
  }

  setMode(mode) {
    this.mode = mode;
    if (mode === 'off') this.stop();
  }

  start() {
    if (this.raf || this.mode === 'off') return;
    const loop = (t) => {
      this.raf = requestAnimationFrame(loop);
      if (t - this.last >= TICK_MS) {
        this.last = t;
        this.draw();
      }
    };
    this.raf = requestAnimationFrame(loop);
  }

  stop() {
    cancelAnimationFrame(this.raf);
    this.raf = 0;
    const { width, height } = this.canvas;
    this.ctx2d.clearRect(0, 0, width, height);
  }

  _size() {
    const dpr = window.devicePixelRatio || 1;
    const w = Math.round(this.canvas.clientWidth * dpr);
    const h = Math.round(this.canvas.clientHeight * dpr);
    if (this.canvas.width !== w || this.canvas.height !== h) {
      this.canvas.width = w;
      this.canvas.height = h;
    }
    return { w, h };
  }

  draw() {
    const { w, h } = this._size();
    const g = this.ctx2d;
    g.clearRect(0, 0, w, h);
    const css = getComputedStyle(this.canvas);
    g.fillStyle = css.getPropertyValue('--viz').trim() || '#6cf';
    g.strokeStyle = g.fillStyle;
    if (this.mode === 'scope') this._scope(g, w, h);
    else this._bars(g, w, h);
  }

  _bars(g, w, h) {
    this.analyser.getByteFrequencyData(this.freq);
    const n = this.bars.length;
    const gap = Math.max(2, w / n / 6);
    const bw = (w - gap * (n - 1)) / n;
    for (let i = 0; i < n; i++) {
      let peak = 0;
      const a = this.edges[i];
      const b = Math.max(a + 1, this.edges[i + 1]);
      for (let k = a; k < b && k < this.freq.length; k++) peak = Math.max(peak, this.freq[k]);
      const v = Math.sqrt(peak / 255) * 255; // perceptual square-root scaling
      this.bars[i] = v > this.bars[i] ? v : decay(this.bars[i]);
      const bh = (this.bars[i] / 255) * h;
      g.fillRect(i * (bw + gap), h - bh, bw, bh);
    }
  }

  _scope(g, w, h) {
    this.analyser.getByteTimeDomainData(this.wave);
    const step = Math.max(1, Math.floor(this.wave.length / w));
    g.lineWidth = Math.max(2, (window.devicePixelRatio || 1) * 1.5);
    g.beginPath();
    for (let x = 0; x < w; x++) {
      let best = 128;
      for (let k = 0; k < step; k++) {
        const s = this.wave[Math.min(this.wave.length - 1, x * step + k)];
        if (Math.abs(s - 128) > Math.abs(best - 128)) best = s; // keep the peak
      }
      const y = h / 2 + ((best - 128) / 128) * (h / 2) * 1.6; // boosted so quiet passages show
      if (x === 0) g.moveTo(x, y);
      else g.lineTo(x, y);
    }
    g.stroke();
  }
}
