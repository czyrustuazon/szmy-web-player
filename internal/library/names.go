package library

import (
	"path"
	"strings"

	"github.com/czyrustuazon/lib-szmy-media-kit/unpack"
)

// FixNames gives everything in the library whose name carries unzip's "#Uxxxx" escapes (left
// by an archive that was unpacked in a non-UTF-8 locale and packed again) its real name. Hidden
// entries (the trash, upload staging) are left alone, and nothing is ever overwritten. It
// returns where each file went (old path to new); with dryRun nothing is renamed and the moves
// are what would happen.
func (l *Library) FixNames(dryRun bool) (map[string]string, error) {
	if l.readOnly && !dryRun {
		return nil, ErrReadOnly
	}
	fix := unpack.NameFix{
		DryRun: dryRun,
		Skip:   func(rel string, _ bool) bool { return strings.HasPrefix(path.Base(rel), ".") },
	}
	return fix.Run(l.root), nil
}
