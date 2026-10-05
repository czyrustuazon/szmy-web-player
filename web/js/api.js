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
  setFavorite: (path, on) => request('POST', '/api/favorite', { path, on }),
  meta: (p) => request('GET', `/api/meta?${q({ p })}`),
  prefetch: (path) => request('POST', '/api/prefetch', { path }),
  deleteTrack: (p) => request('DELETE', `/api/track?${q({ p })}`),
  undo: (token) => request('POST', '/api/undo', { token }),
  getSettings: () => request('GET', '/api/settings'),
  putSettings: (s) => request('PUT', '/api/settings', s),
  getResume: () => request('GET', '/api/resume'),
  putResume: (r) => request('PUT', '/api/resume', r),
  errors: () => request('GET', '/api/errors'),
  streamURL: (p, transcode = false) => `/api/stream?${q(transcode ? { p, transcode: '1' } : { p })}`,
  artURL: (p) => `/api/art?${q({ p })}`,

  // XHR (not fetch) so we get upload progress.
  upload(files, dir, onProgress) {
    return new Promise((resolve, reject) => {
      const form = new FormData();
      for (const f of files) form.append('files', f, f.name);
      const xhr = new XMLHttpRequest();
      xhr.open('POST', `/api/upload${dir ? `?${q({ dir })}` : ''}`);
      xhr.setRequestHeader('X-Requested-With', 'masterplayer');
      xhr.upload.onprogress = (e) => e.lengthComputable && onProgress?.(e.loaded / e.total);
      xhr.onerror = () => reject(new ApiError(0, 'network error'));
      xhr.onload = () => {
        let data = null;
        try {
          data = JSON.parse(xhr.responseText);
        } catch {
          /* ignore */
        }
        if (xhr.status === 401) events.dispatchEvent(new Event('unauthorized'));
        if (xhr.status >= 200 && xhr.status < 300) resolve(data);
        else reject(new ApiError(xhr.status, data?.error || xhr.statusText));
      };
      xhr.send(form);
    });
  },
};
