package addons

// entries.go: the addon list as data (Go-only: `lo addons -o json|yaml`).
// The same rows the table prints, with the same sources. The text table
// (List, ListOrigin) stays byte-identical to the bash and does not go
// through here.

import (
	"io"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
)

// Entry is one addon of the list. Field names are the stable output
// contract (lowerCamel).
type Entry struct {
	Name       string `json:"name" yaml:"name"`
	Type       string `json:"type" yaml:"type"`
	Version    string `json:"version" yaml:"version"`
	Chart      string `json:"chart,omitempty" yaml:"chart,omitempty"`
	Repository string `json:"repository,omitempty" yaml:"repository,omitempty"`
	Origin     string `json:"origin" yaml:"origin"`
	Path       string `json:"path" yaml:"path"`
}

// Entries lists the addons the way List does (the driver check included:
// a non-Lo domain is refused with the same message on stderr). Every row
// carries its origin. A listing never ejects.
func Entries(p *config.Paths, d string, stderr io.Writer) ([]Entry, error) {
	if _, err := Driver(p, d, stderr); err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, name := range Names(p) {
		dir, _, err := assets.Peek(p, "addons/"+name)
		if err != nil {
			continue
		}
		m := readChart(dir)
		e := Entry{Name: name, Type: Type(dir), Version: m.version, Origin: OriginOf(p, name), Path: dir}
		if m.chart != "-" {
			e.Chart, e.Repository = m.chart, m.repository
		}
		out = append(out, e)
	}
	return out, nil
}
