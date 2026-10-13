package lint

// dnssearch.go — spec.network.dnsSearch through the Lo driver's own check,
// so a list that `lo up` rejects fails lint too (bash: lint::dns_search).

import (
	"github.com/kernpilot/lok8s/internal/domain"
	lodriver "github.com/kernpilot/lok8s/internal/driver/lo"
	"github.com/kernpilot/lok8s/internal/ui"
)

// dnsSearch prints one error line per problem of a Lo spec's
// spec.network.dnsSearch (lodriver.CheckDNSSearch). The driver comes from
// domain.SpecDriver, the one .kind reader. Other drivers have no such key.
// Returns the number of errors.
func (l *Linter) dnsSearch(specFile string) int {
	if driver, err := domain.SpecDriver(specFile, ""); err != nil || driver != "lo" {
		return 0
	}
	_, problems := lodriver.CheckDNSSearch(firstDoc(specFile))
	for _, p := range problems {
		ui.ErrorTo(l.ErrOut, "  %s", p)
	}
	return len(problems)
}
