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

**No password, no login:** with `MP_ADMIN_PASSWORD` blank (the default), anyone who can reach
the port can play, upload and delete. That is fine on a private network; set a password in
`.env` to turn the login on.

`make help` lists every target (`dev` for fast rebuilds, `down`, `restart`, `shell`,
`status`, `clean`, `test`, `smoke`, `deploy`).

### Without Docker

```sh
make go-run                 # builds, then runs against ./music and ./data
```

Install [`vgmstream-cli`](https://github.com/vgmstream/vgmstream/releases) and put it on
`PATH` (or set `MP_VGMSTREAM_BIN`) for game formats. Without it everything else still works.

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
| `MP_TRANSCODE_WORKERS` | `2` | Parallel vgmstream decodes |
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

The format is detected from file content first and the extension second, like szmy.

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
| Embedded cover art, generic fallback | Same |
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
| *(new)* | **Favorites** (heart on every track), keyboard shortcuts, Media Session lock-screen controls, installable PWA |

Keyboard (desktop): `Space` play/pause, `←`/`→` seek 5 s (`Shift` = previous/next), `↑`/`↓`
volume, `N`/`P` next/previous, `S` shuffle, `R` repeat, `F` favorite, `L` locate, `/` search, `Esc` close.

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
- Unpacking happens in a hidden scratch folder, and only **audio files** are kept (symlinks,
  scripts, images and the like are dropped), then a lone wrapper folder is removed and the
  result is moved into place. Nothing partial ever shows up in the library.
- Free space is checked **before** the transfer; abandoned partial uploads are cleaned up after
  `MP_UPLOAD_TTL_HOURS`.

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
  leave the library are refused.
- Uploads can only land inside the upload folder; loose files must sniff as audio; names are
  sanitised; nothing is ever overwritten; archive entries cannot escape (Zip Slip) and are
  reduced to audio files; extraction is size-capped against zip bombs (a 7z bomb is limited only
  by the free-space check).
- Cover art is only served if the bytes really are JPEG/PNG/GIF/WebP, with a sandboxing CSP.
- Passwords are stored as salted, iterated SHA-256 hashes. This is a single-user LAN-grade
  login; put it behind HTTPS (and ideally a VPN) if you expose it to the internet.

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
