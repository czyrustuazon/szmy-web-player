# Third-party notices

## vgmstream

The Docker image bundles the prebuilt `vgmstream-cli` binary from the official release
(<https://github.com/vgmstream/vgmstream/releases>, tag and SHA-256 are pinned in the
`Dockerfile`). It decodes BRSTM, BCSTM, BFSTM, ADPCM and the other game-audio formats
that browsers cannot play. masterplayer runs it as a separate process; no vgmstream code
is linked into the Go binary.

vgmstream is distributed under its own license (ISC-style) plus the licenses of the
libraries it embeds. See <https://github.com/vgmstream/vgmstream/blob/master/COPYING>.

## Inspiration

The feature set follows [szmy](https://github.com/czyrustuazon/szmy), a Nintendo 3DS
music player (vgmstream, dr_flac, libopusfile, minimp3). No szmy source code is used.

## Go and browser code

masterplayer has no third-party Go modules and no third-party JavaScript. Its one dependency,
media-kit (`github.com/czyrustuazon/lib-szmy-media-kit`), is a library by the same author.

The UI icons (inline SVG paths in `web/index.html`) are from Google's Material Design
Icons, licensed under the Apache License 2.0
(<https://github.com/google/material-design-icons>).
