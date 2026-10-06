// Pop-out fallback for browsers without Document Picture-in-Picture (Firefox, Safari).
//
// Those browsers can only pop out a <video>, so the cover, title and progress are drawn on a
// canvas and streamed into a muted video that sits over the cover on the player screen.
//   Safari:  our pop-out button calls video.requestPictureInPicture().
//   Firefox: has no API for it; you use Firefox's own picture-in-picture button on the video.
// The browser draws that window's controls, so it only offers play/pause; that is wired to the
// music. The video carries no sound: the music keeps playing through the normal audio path.

import { $ } from './util.js';

const SIZE = 512;
const BAND = 150; // height of the title strip at the bottom

let player = null;
let video = null;
let canvas = null;
let ctx = null;
let art = null; // the cover as a loaded Image
let artSrc = '';
let onChange = () => {};

// Safari and Chrome can say whether the video is popped out; Firefox cannot.
const canTell = 'pictureInPictureElement' in document;
const poppedOut = () => canTell && document.pictureInPictureElement === video;

// The video's own play / pause buttons drive the music. Safari pauses muted videos that are off
// screen, so there those events only count while the video is actually popped out.
function fromVideo(play) {
  if (canTell && !poppedOut()) return syncPlay();
  if (play && !player.playing) player.play();
  else if (!play && player.playing) player.pause();
}

function syncPlay() {
  if (!video || video.hidden) return;
  if (player.playing && video.paused) video.play().catch(() => {});
  else if (!player.playing && !video.paused) video.pause();
}

export function init(p, onToggle) {
  player = p;
  onChange = onToggle;
  canvas = document.createElement('canvas');
  canvas.width = canvas.height = SIZE;
  ctx = canvas.getContext('2d');
  draw();

  video = document.createElement('video');
  video.id = 'fp-video';
  video.className = 'fp-video';
  video.muted = true;
  video.playsInline = true;
  video.title = canTell ? 'Pop out player' : "Use Firefox's picture-in-picture button to pop out the player";
  video.srcObject = canvas.captureStream(); // a new frame whenever the canvas changes
  video.hidden = true;
  $('#fp-art').after(video);

  video.addEventListener('play', () => fromVideo(true));
  video.addEventListener('pause', () => fromVideo(false));
  video.addEventListener('enterpictureinpicture', () => onChange(true));
  video.addEventListener('leavepictureinpicture', () => {
    onChange(false);
    syncPlay();
  });
}

export const isOpen = () => poppedOut();

export async function open() {
  if (!video || video.hidden || poppedOut()) return;
  await video.requestPictureInPicture();
}

export async function close() {
  if (poppedOut()) await document.exitPictureInPicture();
}

// Redraws the frame and matches the video's play state to the music.
export function sync() {
  if (!video) return;
  const has = !$('#fp-play').disabled;
  video.hidden = !has;
  const src = $('#fp-art').src;
  if (src !== artSrc) {
    artSrc = src;
    const img = new Image();
    img.onload = () => {
      if (artSrc !== src) return;
      art = img;
      draw();
    };
    img.src = src;
  }
  draw();
  syncPlay();
}

function fit(text, max) {
  if (ctx.measureText(text).width <= max) return text;
  let s = text;
  while (s.length > 1 && ctx.measureText(`${s}…`).width > max) s = s.slice(0, -1);
  return `${s}…`;
}

function draw() {
  const css = getComputedStyle(document.documentElement);
  const font = getComputedStyle(document.body).fontFamily;
  ctx.fillStyle = '#111418';
  ctx.fillRect(0, 0, SIZE, SIZE);
  if (art?.naturalWidth || art?.width) {
    // Cover-fit the art into the square.
    const w = art.naturalWidth || art.width;
    const h = art.naturalHeight || art.height;
    const s = Math.max(SIZE / w, SIZE / h);
    ctx.drawImage(art, (SIZE - w * s) / 2, (SIZE - h * s) / 2, w * s, h * s);
  }
  if (!video || video.hidden) return;

  const g = ctx.createLinearGradient(0, SIZE - BAND - 40, 0, SIZE);
  g.addColorStop(0, 'rgb(0 0 0 / 0)');
  g.addColorStop(0.35, 'rgb(0 0 0 / .7)');
  g.addColorStop(1, 'rgb(0 0 0 / .85)');
  ctx.fillStyle = g;
  ctx.fillRect(0, SIZE - BAND - 40, SIZE, BAND + 40);

  const pad = 24;
  ctx.textBaseline = 'alphabetic';
  ctx.fillStyle = '#fff';
  ctx.font = `700 32px ${font}`;
  ctx.fillText(fit($('#fp-title').textContent, SIZE - pad * 2), pad, SIZE - 92);
  ctx.fillStyle = 'rgb(255 255 255 / .75)';
  ctx.font = `22px ${font}`;
  ctx.fillText(fit($('#fp-artist').textContent, SIZE - pad * 2), pad, SIZE - 60);

  // Progress bar with the times under it.
  const frac = Number($('#fp-seek').value) / 1000;
  const barY = SIZE - 42;
  ctx.fillStyle = 'rgb(255 255 255 / .25)';
  ctx.fillRect(pad, barY, SIZE - pad * 2, 6);
  ctx.fillStyle = css.getPropertyValue('--accent').trim() || '#5ab0ff';
  ctx.fillRect(pad, barY, (SIZE - pad * 2) * frac, 6);
  ctx.fillStyle = 'rgb(255 255 255 / .75)';
  ctx.font = `18px ${font}`;
  ctx.fillText($('#fp-cur').textContent, pad, SIZE - 12);
  const dur = $('#fp-dur').textContent;
  ctx.fillText(dur, SIZE - pad - ctx.measureText(dur).width, SIZE - 12);
}
