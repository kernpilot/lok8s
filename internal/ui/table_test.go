package ui

import (
	"bytes"
	"fmt"
	"testing"
)

// Piped with minimum widths, Table reproduces the fixed printf layout of
// the ported commands byte for byte: an overflowing cell pushes only its
// own row, like bash printf '%-20s'.
func TestTablePipedMatchesPrintf(t *testing.T) {
	header := []string{"NAME", "TYPE", "VERSION", "CHART/REPO"}
	rows := [][]string{
		{"cilium", "helm", "1.16.0", "cilium/cilium"},
		{"kube-prometheus-stack", "helm", "65.1.1", "prometheus-community/kube-prometheus-stack"},
		{"metallb", "manifest", "", "manifests/"},
	}
	var want bytes.Buffer
	f := "%-20s  %-8s  %-12s  %s\n"
	fmt.Fprintf(&want, f, "NAME", "TYPE", "VERSION", "CHART/REPO")
	fmt.Fprintf(&want, f, "----", "----", "-------", "----------")
	for _, r := range rows {
		fmt.Fprintf(&want, f, r[0], r[1], r[2], r[3])
	}
	var got bytes.Buffer
	Table(&got, header, rows, []int{20, 8, 12})
	if got.String() != want.String() {
		t.Errorf("piped table differs from printf:\n--- got\n%s--- want\n%s", got.String(), want.String())
	}
}

// Without minimum widths (the Go-only tables) the columns are measured.
func TestTablePipedMeasured(t *testing.T) {
	var got bytes.Buffer
	Table(&got, []string{"ASSET", "KIND"}, [][]string{{"addons/cilium", "addon"}, {"tilt", "tilt"}}, nil)
	want := "ASSET          KIND\n-----          ----\naddons/cilium  addon\ntilt           tilt\n"
	if got.String() != want {
		t.Errorf("got %q, want %q", got.String(), want)
	}
}

// On a TTY the columns are measured (never narrower than the minimum) so
// no cell overflows, and the header is bold, the underline dim.
func TestTableTerminalNeverOverflows(t *testing.T) {
	defer ForceTTY(true)()
	defer ForceColor(true)()
	var got bytes.Buffer
	Table(&got, []string{"NAME", "TYPE"}, [][]string{{"kube-prometheus-stack", "helm"}, {"x", "y"}}, []int{4, 8})
	want := "\033[1mNAME                   TYPE\033[0m\n" +
		"\033[2m----                   ----\033[0m\n" +
		"kube-prometheus-stack  helm\n" +
		"x                      y\n"
	if got.String() != want {
		t.Errorf("got %q, want %q", got.String(), want)
	}
}

func TestTableShortRow(t *testing.T) {
	var got bytes.Buffer
	Table(&got, []string{"A", "B", "C"}, [][]string{{"1"}}, nil)
	if got.String() != "A  B  C\n-  -  -\n1     \n" {
		t.Errorf("got %q", got.String())
	}
}
