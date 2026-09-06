package assets

// update.go — `lo assets update <rel> [--force]`: apply the embedded copy
// over the project's, but only when every file is provably untouched
// (local == origin). The diff is shown first, the marker rewritten after.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kernpilot/lok8s/internal/config"
)

// ErrConflict marks an update refused because the project changed a file
// (or no marker proves it did not).
var ErrConflict = errors.New("assets: local changes would be overwritten")

// Update applies the embedded unit covering rel over the local copy.
// Refuses (ErrConflict, nothing written) when any file is local modified
// or both, or when the unit carries no marker — unless force. Files that
// exist only locally are kept. Returns the report it acted on.
func Update(p *config.Paths, rel string, force bool, out io.Writer) (UnitReport, error) {
	reports, err := Report(p, []string{rel})
	if err != nil {
		return UnitReport{}, err
	}
	r := reports[0]
	switch r.Origin {
	case OriginColBuiltin:
		fmt.Fprintf(out, "%s: not ejected — the binary's copy is what runs; nothing to update\n", r.Rel)
		return r, nil
	case OriginColLocalOnly:
		return r, fmt.Errorf("%w: %s (the binary ships no copy)", ErrNotAsset, r.Rel)
	}
	WriteShow(out, r)
	if !r.Drifted {
		if r.Marker != nil && !force {
			fmt.Fprintf(out, "\n%s: already in sync\n", r.Rel)
			return r, nil
		}
	}
	c := r.Counts()
	conflicts := c[StateLocalModified] + c[StateBoth]
	if !force {
		if r.Marker == nil {
			return r, fmt.Errorf("%w: %s has no %s marker, so local edits cannot be told apart from lo updates (re-run with --force to replace it with the binary's copy)", ErrConflict, r.Rel, MarkerFile)
		}
		if conflicts > 0 {
			return r, fmt.Errorf("%w: %s has %d locally modified file(s); resolve them or re-run with --force", ErrConflict, r.Rel, conflicts)
		}
	}
	u, _ := UnitFor(r.Rel)
	dir := localPath(p, u.Rel)
	written := 0
	err = fs.WalkDir(FS(), u.Rel, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(fp, u.Rel+"/")))
		data, err := fs.ReadFile(FS(), fp)
		if err != nil {
			return err
		}
		if cur, err := os.ReadFile(target); err == nil && string(cur) == string(data) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		written++
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return r, err
	}
	m, err := markerFor(u)
	if err != nil {
		return r, err
	}
	if err := m.write(filepath.Join(dir, MarkerFile)); err != nil {
		return r, err
	}
	fmt.Fprintf(out, "\n%s: updated %d file(s) from lo %s; %s rewritten\n", r.Rel, written, Version(), MarkerFile)
	return r, nil
}

func readEmbedded(rel string) ([]byte, error) {
	return fs.ReadFile(FS(), rel)
}
