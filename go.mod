module masterplayer

go 1.27

// media-kit is downloaded from GitHub at this version (go.sum pins its contents). With a
// checkout at ../media-kit, `make sync-media-kit` writes a go.work (not committed) that uses it.
require github.com/czyrustuazon/lib-szmy-media-kit v0.1.2
