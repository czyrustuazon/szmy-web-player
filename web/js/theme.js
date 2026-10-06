// Theme picker. Loaded as a classic script in <head> so the saved theme is on
// <html data-theme> before first paint (no flash). The choice is per device, so
// it lives in localStorage rather than the server-side settings.
//
// Choices: system (follow the OS), dark, light, night (warm, dim, low blue
// light) and auto-night (night from 22:00 to 07:00, system otherwise).
(() => {
  const KEY = 'mmp-theme';
  const CHOICES = ['system', 'dark', 'light', 'night', 'auto-night'];
  const BAR = { dark: '#111418', light: '#f5f7fa', night: '#0b0806' };
  const lightOS = matchMedia('(prefers-color-scheme: light)');

  function load() {
    try {
      const v = localStorage.getItem(KEY);
      return CHOICES.includes(v) ? v : 'system';
    } catch {
      return 'system';
    }
  }

  function isNightHour() {
    const h = new Date().getHours();
    return h >= 22 || h < 7;
  }

  let choice = load();

  function apply() {
    let t = choice;
    if (t === 'auto-night') t = isNightHour() ? 'night' : 'system';
    const root = document.documentElement;
    if (t === 'system') root.removeAttribute('data-theme');
    else root.setAttribute('data-theme', t);
    const shown = t === 'system' ? (lightOS.matches ? 'light' : 'dark') : t;
    const meta = document.querySelector('meta[name="theme-color"]');
    if (meta) meta.content = BAR[shown];
  }

  lightOS.addEventListener('change', apply);
  // Auto-night has to flip on its own at 22:00 / 07:00.
  setInterval(() => choice === 'auto-night' && apply(), 60_000);
  document.addEventListener('visibilitychange', () => !document.hidden && apply());

  window.mmpTheme = {
    get: () => choice,
    set(v) {
      choice = CHOICES.includes(v) ? v : 'system';
      try { localStorage.setItem(KEY, choice); } catch { /* private mode: session only */ }
      apply();
    },
  };
  apply();
})();
