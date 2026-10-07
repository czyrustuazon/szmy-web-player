const HEADERS = { 'X-Requested-With': 'masterplayer' };

export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

export const events = new EventTarget();

async function request(method, url, body) {
  const init = { method, headers: { ...HEADERS }, credentials: 'same-origin' };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const res = await fetch(url, init);
  if (res.status === 401 && !url.startsWith('/api/login')) events.dispatchEvent(new Event('unauthorized'));
  if (!res.ok) {
    let msg = res.statusText;
    try {
      msg = (await res.json()).error || msg;
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, msg);
  }
  if (res.status === 204 || res.status === 202) return null;
  return res.json();
}

const q = (params) => new URLSearchParams(params).toString();

export const api = {
  session: () => request('GET', '/api/session'),
  login: (password) => request('POST', '/api/login', { password }),
  logout: () => request('POST', '/api/logout', {}),
  browse: (dir) => request('GET', `/api/browse?${q({ dir })}`),
  tracks: (dir = '') => request('GET', `/api/tracks?${q({ dir })}`),
  favorites: () => request('GET', '/api/favorites'),
  played: (path) => request('POST', '/api/played', { path }),
  setFavorite: (path, on) => request('POST', '/api/favorite', { path, on }),
  talk: () => request('GET', '/api/talk'),
  setTalk: (path, on) => request('POST', '/api/talk', { path, on }),
  meta: (p) => request('GET', `/api/meta?${q({ p })}`),
  prefetch: (path) => request('POST', '/api/prefetch', { path }),
  deleteTrack: (p) => request('DELETE', `/api/track?${q({ p })}`),
  renameFolder: (path, name) => request('POST', '/api/rename', { path, name }),
  mergeFolder: (path, into) => request('POST', '/api/merge', { path, into }),
  planNames: () => request('GET', '/api/fixnames'),
  fixNames: () => request('POST', '/api/fixnames'),
  undo: (token) => request('POST', '/api/undo', { token }),
  getSettings: () => request('GET', '/api/settings'),
  putSettings: (s) => request('PUT', '/api/settings', s),
  getResume: () => request('GET', '/api/resume'),
  putResume: (r) => request('PUT', '/api/resume', r),
  errors: () => request('GET', '/api/errors'),
  streamURL: (p, transcode = false) => `/api/stream?${q(transcode ? { p, transcode: '1' } : { p })}`,
  artURL: (p) => `/api/art?${q({ p })}`,
};
