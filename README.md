# Master Music Player

A self-hosted web music player that brings [szmy](https://github.com/czyrustuazon/szmy)'s
features to the browser: it plays normal formats **and** game music (BRSTM, BCSTM, BFSTM,
ADPCM and anything else [vgmstream](https://github.com/vgmstream/vgmstream) decodes),
including **sample-accurate loop points**. Phone-first layout, installable as a PWA.

One Go binary (standard library only) with the web app embedded. Runs in Docker.

> **Status:** written without a Go toolchain or Docker available, so the Go code and the
> container have **not been compiled or run yet**. The browser logic is tested with Node
> (`make web-test`). Run `make test` and `make smoke` first; see [First run](#first-run-checklist).
> The Makefile follows the `anime-db-stream` template (`make help` lists the targets).

## Quick start

### Docker

```sh
cp .env.example .env        # set LIBRARY_HOST_PATH, PUID and PGID (`id -u`, `id -g`)
make up                     # build from scratch and start
make logs                   # follow the logs
```

Open <http://localhost:8787> (change the host port with `PORT` in `.env`). The app opens on the
**player**, even when the library is empty; use the **Upload** tab to add music.

**Who can connect:** by default only your own network, like `animedb.haruhi.one`: devices on
your **Tailscale network** and on your **home network**. Everything else, including the public
internet through a router port-forward, is refused with a 403 (see [Who can connect](#who-can-connect)).

**No password, no login:** with `MP_ADMIN_PASSWORD` blank (the default), anyone *who is allowed to
connect* can play, upload and delete. Combined with the access rules above that is usually what you
want on a private network; set a password in `.env` to add a login on top.

`make help` lists every target (`dev` for fast rebuilds, `down`, `restart`, `shell`,
`status`, `clean`, `test`, `smoke`, `deploy`).

### Without Docker

```sh
make go-run                 # builds, then runs against ./music and ./data
```

Install [`vgmstream-cli`](https://github.com/vgmstream/vgmstream/releases) and put it on
`PATH` (or set `MP_VGMSTREAM_BIN`) for game formats, and `ffmpeg` (with `ffprobe`) for WMA, APE and
similar formats. Without them everything else still works. The Docker image has both.

## Configuration

All settings are environment variables.

| Variable | Default | Meaning |
|---|---|---|
| `MP_PORT` | `8080` | HTTP port |
| `MP_MUSIC_DIR` | `./music` (`/music` in Docker) | Library folder |
| `MP_DATA_DIR` | `./data` (`/data` in Docker) | State file, transcode cache, error log |
| `MP_ADMIN_PASSWORD` | *(blank)* | Login password. **Blank means open access (no login)**; a warning is logged at startup |
| `MP_READ_ONLY` | `false` | Force read-only: delete and upload are disabled even if the folder is writable (a folder that is not writable is detected automatically) |
| `MP_COOKIE_SECURE` | `false` | Mark the session cookie `Secure`; set `true` behind HTTPS |
| `MP_UPLOAD_SUBDIR` | `uploads` | Folder (inside the library) that uploads go to |
| `MP_MAX_UPLOAD_MB` | `61440` | Largest single upload, and largest archive once unpacked (60 GiB) |
| `MP_MIN_FREE_MB` | `1024` | Free space that must remain on the library disk after an upload (`0` = no check) |
| `MP_UPLOAD_TTL_HOURS` | `48` | An upload nobody resumed is cleaned up after this long |
| `MP_CACHE_MB` | `2048` | Transcode cache size (least recently used files are evicted) |
| `MP_TRANSCODE_WORKERS` | `2` | Parallel vgmstream and ffmpeg conversions |
| `MP_TRASH_MINUTES` | `10` | How long a deleted file stays restorable |
| `MP_VGMSTREAM_BIN` | `vgmstream-cli` | Path or name of the vgmstream binary |
| `PUID` / `PGID` | `1000` | Docker only: user and group the app runs as |

Docker volumes: `/music` (your library; deleting needs it writable), `/music/uploads` (where uploads
are stored, see [Where uploads are stored](#where-uploads-are-stored)) and `/data`. Mount `/music` with
`:ro` for a library that cannot be deleted from; uploads still work because they have their own folder. TLS is left to a reverse proxy
(Caddy, nginx, Traefik).

## Formats

| Format | How it plays |
|---|---|
| MP3, FLAC, WAV, Ogg Vorbis, Opus, M4A/AAC | Streamed as-is with HTTP Range (seeking works). Opus on browsers without Ogg Opus support is transcoded automatically |
| BRSTM, BCSTM, BFSTM, ADPCM and other vgmstream formats | Rendered once to WAV by `vgmstream-cli`, cached, then streamed |
| WMA, WMV/ASF, APE, WavPack, TTA, Musepack, DSD, AIFF, MKA, FLV, AMR, AC3/DTS and other video/audio containers | Converted once to FLAC by `ffmpeg` (lossless, seekable), cached, then streamed. Title, artist, album, genre, year and track come from `ffprobe` |

The format is detected from file content first and the extension second, like szmy. MIDI is not
supported (it has no audio to decode). Converted files are cached under `MP_DATA_DIR/cache`
(`MP_CACHE_MB` is the limit for each cache) and a conversion that takes over 15 minutes is stopped.

### Loop points

Loop regions come from vgmstream (game formats), `LOOPSTART` plus `LOOPEND` or `LOOPLENGTH`
Vorbis comments (FLAC, Ogg, Opus) or a WAV `smpl` chunk. A looping track is decoded in the
browser and played through Web Audio with `loopStart`/`loopEnd`, which loops gaplessly.
Settings choose what happens at the end of the intro: **loop N times then fade out**
(default 2, 10 s fade, vgmstream's behaviour), **loop forever**, or **play once**.

## szmy features, in the browser

| szmy | Here |
|---|---|
| Format detection from content, then extension | Same (`internal/sniff`) |
| Folders first, case-insensitive sort, hidden files skipped | Same; no 128-entry cap (virtualised list) |
| Non-audio files are rejected with a message | Toast |
| Shuffle from a no-repeat bag; bad files skipped | Same (`web/js/queue.js`); bad files skipped on auto-advance, at most 5 in a row |
| Repeat off / all / one on one button | Same |
| Auto-advance crosses folders | The queue is the whole library (or Favorites) |
| Delete with automatic advance to the next track | One tap on the trash button, plus a 5 s **Undo** (files go to a hidden trash first) |
| Tap-to-return playlist cursor | Tap the title in the player, or the locate button |
| Resume where you left off | Track, position, shuffle, repeat, volume and loop settings persist on the server |
| Embedded cover art, generic fallback | Same, plus a picture from the album folder when the track has none (below) |
| Scrolling long titles | Marquee ticker |
| ID3v2 (Latin-1/UTF-8/UTF-16), Vorbis comments, genre `(17)Rock` cleanup, 4-digit year | Same, plus WAV `LIST/INFO`; filename fallback title; CJK font stack |
| Equalizer bars (80 Hz–12 kHz, log bands, fast rise / half-plus-8 fall) and scope | Same, drawn from a Web Audio analyser; tap to switch |
| Accurate seek bar | Same |
| Toasts | Same |
| Keeps the console awake while playing | Screen Wake Lock |
| L/R double-tap guard against pocket presses | Optional double-press guard for lock-screen next/previous |
| FTP server with a random readable password | The **Upload** tab (tap or drag and drop), chunked and resumable; password-protected when `MP_ADMIN_PASSWORD` is set |
| Structured error log (`code msg site path`) | `data/error.log` with rotation, viewable in Settings |
| Source selector | Library / Favorites tabs |
| *(new)* | **Fuzzy search** on the Library and Favorites screens (see below) |
| Konami code | Yes |
| *(new)* | **Favorites** (heart on every track) and a **Talk** list for speech-only files (microphone on every track), keyboard shortcuts, Media Session lock-screen controls, installable PWA |

Keyboard (desktop): `Space` play/pause, `←`/`→` seek 5 s (`Shift` = previous/next), `↑`/`↓`
volume, `N`/`P` next/previous, `S` shuffle, `R` repeat, `F` favorite, `T` talk, `L` locate, `/` search, `Esc` close.

## Who can connect

The player is meant for you and your devices, the same as `animedb.haruhi.one` and
`anime-db-stream`. Every connection is checked, before anything else happens (login page and static
files included), by the address it **really** comes from. Headers such as `X-Forwarded-For` are
ignored, because anyone can forge them. The one exception is a tunnel or reverse proxy running on
your own machine or home network (`MP_TRUSTED_PROXIES`): it is the only one that knows the visitor's
address, so the player believes its `CF-Connecting-IP` / `X-Forwarded-For` and checks that address
against `MP_TUNNEL_NETS`.

| Setting | Meaning |
|---|---|
| `MP_ALLOWED_NETS` | Networks that may connect. Default `tailscale,lan`: your Tailscale network (`100.64.0.0/10`) and private home-network ranges. Add or swap in your own CIDR such as `192.168.1.0/24`. Use just `tailscale` for the tailnet only. Loopback is always allowed (health checks). |
| `MP_KNOWN_DEVICES` | Optional. Names of your devices: their MagicDNS names, as in `tailscale status`. Tailscale peers must then be one of them. The name comes from this host's own `tailscaled`, and it is the device's MagicDNS name, never the hostname a device reports about itself, so renaming a machine does not get it in. A device shared in from another tailnet matches only its full name (`laptop.other-tailnet.ts.net`), so it cannot pass for one of yours. Needs MagicDNS turned on. Home-network devices are covered by `MP_ALLOWED_NETS`. Needs `tailscaled` on the host: the Makefile then mounts its socket via `docker-compose.tailscale.yml`. |
| `MP_TRUSTED_PROXIES` | Where a tunnel or reverse proxy may connect from. Default `loopback,lan` (`cloudflared` on this host reaches the container from loopback or the Docker gateway). A request from there that carries a forwarding header is judged by the visitor address in that header instead. Headers from anywhere else (a tailnet peer, the internet) are still ignored. `none` never reads them. |
| `MP_TUNNEL_NETS` | Where visitors arriving through such a proxy may come from. Default `any`, so a Google-sign-in tunnel works from anywhere. Narrow it (same syntax as `MP_ALLOWED_NETS`, e.g. your home's public address or your carrier's range) to put a network check back in front of the sign-in. A proxied request with no readable visitor address is refused. |
| `BIND_ADDR` | Host address the port is published on. `0.0.0.0` (default) is every IPv4 interface; set it to the host's Tailscale address (`tailscale ip -4`) to listen on the tailnet only, like `BIND_ADDR` in `anime-db-stream`. |

To reach it by name, as with `animedb.haruhi.one`, add a DNS record for a name of your choice that
points to the server's Tailscale address. Only devices on your tailnet can route to that address.

Notes:

- The port is published on IPv4 only on purpose. Docker's IPv6 forwarding hides the real client
  address, which would defeat the check.
- With no login configured (no password, no Google sign-in), any request that says it came through
  a proxy or tunnel is refused with a 403, whatever its address. Without a login the network check
  is the only lock, and a tunnel on this machine would otherwise make the whole internet look local.
  See [the Cloudflare Tunnel section](#reaching-it-through-a-cloudflare-tunnel-google-sign-in).
- A refusal is logged once per address per ten minutes, so a scanner cannot flood the log.
- "At home" means the home network's private address ranges. A Tailscale device that is away from home
  reaches you through the tailnet and is allowed like any tailnet device (or only if it is a known
  device, when `MP_KNOWN_DEVICES` is set). To allow the tailnet but not direct home-network
  connections, use `MP_ALLOWED_NETS=tailscale`.

## Reaching it through a Cloudflare Tunnel (Google sign-in)

A tunnel makes the player reachable from anywhere, so the network allowlist above stops being the
lock: every visitor arrives from `cloudflared` on your own machine. **Google sign-in becomes the
lock instead.** Only the addresses in `MP_ALLOWED_EMAILS` get a session; any other Google account,
and anyone not signed in, sees nothing but the sign-in page.

1. **Google Cloud console** → APIs & Services → Credentials → *Create credentials* → *OAuth client ID*,
   type *Web application*. Under *Authorized redirect URIs* add
   `https://music.haruhi.one/auth/google/callback` (your public address plus `/auth/google/callback`;
   the player prints the exact value at startup). If asked, set the consent screen up as *External*
   and add yourself as a test user; the `openid email` scope needs no review.
2. **`.env`**:

   ```
   MP_GOOGLE_CLIENT_ID=...apps.googleusercontent.com
   MP_GOOGLE_CLIENT_SECRET=...
   MP_ALLOWED_EMAILS=you@gmail.com,someone.else@gmail.com
   MP_PUBLIC_URL=https://music.haruhi.one
   ```

   The player refuses to start if any of these is missing, so it can never be left open to every
   Google account. With `MP_ADMIN_PASSWORD` blank, Google is the only way in; with both set, the
   login page offers both.
3. **Tunnel**: in Cloudflare Zero Trust → Networks → Tunnels, add a public hostname
   `music.haruhi.one` → service `http://localhost:8787` (or `http://<BIND_ADDR>:8787` if you set
   `BIND_ADDR` to the Tailscale address). Cloudflare provides the HTTPS side; the player sees plain
   http from the tunnel and marks its cookies Secure because `MP_PUBLIC_URL` is https.
4. `make up`.

Notes:

- Keep the default `MP_ALLOWED_NETS` and `MP_TRUSTED_PROXIES`. Tunnel connections come from this
  machine, so they are judged by the visitor address `cloudflared` passes on, against
  `MP_TUNNEL_NETS`. Its default, `any`, means **Google sign-in carries all of the protection** for
  tunnel visitors. `MP_KNOWN_DEVICES` does not apply to them (they are not Tailscale peers).
- **Recommended: put Cloudflare Access in front** (next section).
- Turning the login off (no password and no Google) does not open the tunnel: every request through
  it is refused (see [Who can connect](#who-can-connect)).
- The address must be listed exactly (case does not matter), and Google must have verified it.
  `you+tag@gmail.com` and `y.o.u@gmail.com` are different entries from `you@gmail.com`.
- Sessions last 30 days. Remove an address and restart to cut it off; restarting also ends every
  session.
- Pointing `music.haruhi.one` at the tunnel replaces its Tailscale DNS record. Other names, such as
  `animedb.haruhi.one`, are unaffected.

### Cloudflare Access in front (recommended)

Cloudflare Access checks who you are at Cloudflare's edge, before a request reaches your machine.
Strangers never touch the player at all (not even its static files or sign-in routes), and the
player's own Google sign-in becomes a second lock instead of the only one. The free plan covers it.

1. **dash.cloudflare.com → Zero Trust** (the dashboard calls it *Cloudflare One*). The first time,
   pick a team name (it becomes `<team>.cloudflareaccess.com`) and the Free plan. Skip the
   "What would you like to do next?" cards; everything is in the left sidebar.
2. **Access controls → Applications → Add an application → Self-hosted.** Domain
   `music.haruhi.one`, path empty (the whole site). Set the session duration to something long,
   such as a month: the app is a PWA, and when the Access session runs out its background requests
   fail until you reload.
3. **Login method.** *One-time PIN* (Cloudflare emails a code) is on by default and is enough. To
   click through with Google instead, add it under **Integrations → Identity providers → Google**,
   with an OAuth client whose redirect URI is
   `https://<team>.cloudflareaccess.com/cdn-cgi/access/callback`, then tick it on the
   application's Authentication screen.
4. **Policy: who may get in.** Action *Allow*, **Include → Emails** → your address(es), the same
   ones as `MP_ALLOWED_EMAILS`. Not *Everyone*, and not *Emails ending in* `@gmail.com`, which
   would let any Gmail user past this lock.
5. **Attach the policy to the application and save it.** A policy made under Access controls →
   Policies does nothing until it is listed on the application's Policies tab. Symptom when it is
   missing: the one-time PIN never arrives, because Cloudflare only emails codes to addresses a
   policy on the application allows (and shows the same "code sent" page either way).

Check it from a private window: `music.haruhi.one` should send you to
`<team>.cloudflareaccess.com`, your email should get a code and then the player's own sign-in, and
any other email should get no code. From outside, every path (including `/api/...`, `/healthz`
and `/auth/google/...`) should answer with a redirect to the Access login, never with the player.
Optionally turn on HSTS under SSL/TLS → Edge Certificates; the player does not send it itself.

## Search

The Library and Favorites screens have a search box (press `/` anywhere to jump to it). Results
update as you type and the matching letters are highlighted. Searching looks at the **file name
and its folder path**, so artist and album folders match too, and it forgives mistakes:

- every word must match, in any order: `zelda storms` finds "Zelda / Song of Storms";
- letters in order are enough: `fnlfntsy` finds "Final Fantasy";
- small typos and swapped letters are fine: `fnial fantsy`, `aeirth`;
- case and accents are ignored; Japanese and other scripts match as written.

Playing a result queues the results in ranked order. Tags (title, artist) are not searched, because
reading them for the whole library on every search would be slow; folder and file names carry that
information for most collections.

## Uploading

The **Upload** tab accepts audio files and `.zip` / `.7z` archives of any size. It uses the
same chunked, resumable protocol as `anime-db-stream`:

- A file is sent as 16 MiB chunks, each its own short request carrying a CRC32 that the server
  verifies before writing. No single connection has to survive the whole transfer.
- The **disk on the server is the source of truth** for progress. After any failure (dropped
  connection, bad checksum, a reload, two tabs) the client asks "how many bytes do you have?"
  and carries on from there. Choose the same files again after a reload and only the missing
  part is sent.
- Archives are verified and unpacked **in the background**; the page polls and shows progress,
  so no request is held open for the length of an extraction.
- A damaged archive is **localised to the bad chunk** (only that part is re-sent). If every chunk
  matches what arrived, the source file itself is bad and is reported as such.
- Unpacking happens in a hidden scratch folder, and only **audio files** and up to three real
  **pictures per folder** (cover-named ones first, see below) are kept. Symlinks, scripts, text
  files and everything else are dropped, then a lone wrapper folder is removed and the result is
  moved into place. Nothing partial ever shows up in the library.
- The result says what was dropped, by type (`jpg ×927, txt ×66, ...`), and links to the **full
  list of skipped files** (`GET /api/upload/report`). Reports are kept next to the staging area
  and cleaned up with abandoned uploads after `MP_UPLOAD_TTL_HOURS`.
- Free space is checked **before** the transfer; abandoned partial uploads are cleaned up after
  `MP_UPLOAD_TTL_HOURS`.

### Cover art

A track's embedded picture is used first. If it has none, the player looks in the track's folder
for an image, preferring `cover`, `folder`, `front`, `album`, `albumart`, `art`, `artwork` and
`thumb`, then in `Scans`, `Artwork`, `Covers`, `Art`, `Images` or `Booklet` subfolders, then in
the parent folder (so a multi-disc album's `CD1`/`CD2` share the cover above them). Pictures
are not listed as files in the library, and only real JPEG, PNG, GIF and WebP images up to
16 MiB are used; symlinks and hidden files are ignored.

### Adding to something you already uploaded

Tick **Add to the folder with this name if it already exists** (Upload tab) and type the folder's
name: the upload then joins that folder instead of creating `name (2)`. Nothing is overwritten:

- a folder that already exists is merged into, and a lone folder inside the archive that matches
  one already there is merged rather than unwrapped, so a few new files for one album land in that
  album;
- a file with the same name **and identical bytes** is skipped and counted as "already in the
  library"; the same name with different bytes is kept as `name (2).ext`;
- the result says how many were new and how many were already there.

Duplicates are found by path and content inside that folder; a file that was renamed, or sits in
a different folder, is not recognised as a copy. Without the option every upload gets a fresh folder.

To add only what an earlier upload dropped (cover pictures, WMA/APE files, archives inside the
archive) without sending the whole thing again, build a small zip from the unpacked folder on your
computer (needs 7-Zip and Node):

```sh
node scripts/delta-for-upload.mjs "E:Z_Gen Musicmusic" delta.zip
```

It takes up to three real pictures per folder, audio in the ffmpeg formats, and unpacks archives found
inside the folders (everything in those is new, because the server never opens archives inside
archives). Upload `delta.zip` with the same folder name and the merge option ticked.

### Merging folders

To merge two folders that are already in the library, **rename** one of them to the exact name of the
other (same parent folder). The player then asks whether to merge. Everything in the first folder
moves into the second and the first is removed. The rules are the same as for uploads: sub-folders
that exist in both are merged, identical files are dropped, a file with the same name but other
content is kept as `name (2).ext`, nothing is overwritten. Favorites and play counts follow the
files, and the queue is updated. Hidden files and links are left behind (and the folder with them).
There is no undo, but only identical duplicates are ever removed. The API is
`POST /api/merge {path, into}`, which accepts any two folders that are not inside one another.

### Where uploads are stored

Uploads are stored on the **host** in `UPLOADS_HOST_PATH` (`.env`), which defaults to `./uploads`,
a folder next to this project, so on the **main disk**. Inside the app it appears as `uploads/` in
the library. To store uploads elsewhere (a big drive, say) set it in `.env` and run `make up`:

```sh
UPLOADS_HOST_PATH=/mnt/anime-drive/music-uploads
```

Unfinished uploads are staged in a hidden `.uploads` folder **inside that same folder**, so a large
upload never fills a different disk first and the final step is an instant rename. `make up` creates
the folder as your user; the container also makes sure the app user can write to it.

Files land in `uploads/<folder name>/` (or straight in `uploads/` when the name is blank; a
lone archive defaults to a folder named after itself). `.7z` needs the `7z` binary, which the
Docker image includes; without it the page says so before anything is sent. Behind a reverse
proxy, allow request bodies of at least 16 MiB.

## Security notes

- With a password set, every request needs the session cookie (`HttpOnly`,
  `SameSite=Strict`). With none, access is open. In both modes state-changing requests need
  an `X-Requested-With` header, which cross-site forms cannot send.
- All paths are resolved against the library root; `..`, hidden folders and symlinks that
  leave the library are refused, including for files that do not exist yet (a new file under a
  symlinked folder that leads out of the library is refused too).
- Uploads can only land inside the upload folder; loose files must sniff as audio; names are
  sanitised; nothing is ever overwritten; archive entries cannot escape (Zip Slip) and are
  reduced to audio files; extraction is size-capped against zip and 7z bombs (`MP_MAX_UPLOAD_MB`).
  A `.7z` is listed before it is unpacked and refused if it holds a symbolic or hard link, a path
  that climbs out, or more data than the limit.
- Cover art (embedded or from a folder) is only served if the bytes really are JPEG/PNG/GIF/WebP,
  with a sandboxing CSP.
- ffmpeg reads untrusted media: it runs with `-protocol_whitelist file` (no network or other
  protocols can be reached through a crafted playlist or container) and `-format_whitelist`
  (only audio/video containers, so a file cannot be read as an HLS or concat playlist that pulls
  other files from the disk into the audio), without stdin, under a 15 minute limit, and its
  output goes to a private cache.
- Passwords are stored as salted, iterated SHA-256 hashes. After 10 wrong passwords from one
  address (one /64 for IPv6; behind a tunnel, the visitor's address) that address is refused for
  15 minutes, and at most two password checks run at once, so a flood of guesses cannot hog the
  CPU. This is a single-user login; prefer Google sign-in (and
  [Cloudflare Access](#cloudflare-access-in-front-recommended)) for the internet.
- Sign in with Google keeps nothing on the server for a sign-in in progress: the PKCE verifier
  and nonce travel in a signed, `HttpOnly` cookie, so strangers starting sign-ins cannot fill up a
  table and lock you out.
- Visitors who are not signed in only learn how to sign in from `/api/session`, not what the
  server can do (upload folder, size limit, installed tools).

## Development

```sh
make test         # go vet + go test + coverage gate (COVER_MIN, default 100) in a container
make smoke        # build the image, start it, check login/browse/range streaming
make web-test     # Node tests for the browser logic (needs Node 20+)
make cover        # the coverage gate on the host (needs Go)
make deploy       # git pull + test + up, on the Ubuntu host
```

Layout: `cmd/masterplayer` (entry point), `internal/{config,sniff,meta,library,transcode,store,auth,errlog,api}`,
`web/` (embedded app: plain ES modules, no build step), `tests/web` (Node tests).

### First-run checklist

1. `make test` (go vet, go test and the coverage gate in a container) and fix anything the
   compiler finds. Statement coverage of `internal/` is 100% and the gate enforces it, like
   szmy's mandate; lower it temporarily with `make test COVER_MIN=90` while iterating.
2. `make dev`, then open the UI (it goes straight to the library when no password is set).
3. Check vgmstream's flags against your build: the app calls `vgmstream-cli -m FILE` (metadata,
   loop start/end) and `vgmstream-cli -i -o OUT.wav FILE` (decode once, ignoring the loop).
   Parsing is in `internal/transcode/transcode.go` (`ParseMetadata`).
4. `make smoke`, then play a BRSTM and a looping track in a real browser (Chrome and Safari).

## Design decisions

- **Go, one package.** There is one app, one data store and one deployment, so no
  frontend/backend split.
- **JSON state file instead of SQLite.** Favorites, settings and resume are tiny; a JSON file
  written atomically keeps the module free of dependencies. Swap in SQLite if a library of
  playlists ever outgrows it.
- **Favorites are keyed by path**, so renaming a file outside the app drops its favorite.
- **Uploads have their own mount inside the library** (`UPLOADS_HOST_PATH` at `/music/uploads`):
  they show up in the library with no second root to manage, they can live on another disk, and
  they keep working when the rest of the library is read-only.
- **Loop playback decodes the whole file in the browser.** Fine for game music (a few MB);
  a 10-minute stereo track needs roughly 100 MB of RAM on the phone.
