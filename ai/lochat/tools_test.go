package main

import (
	"strings"
	"testing"
)

func TestCatalogTiersAndDiet(t *testing.T) {
	tools := []Tool{
		{Name: "lo_status", Description: "status line\nsecond line"},
		{Name: "lo_build", Description: "build"},
		{Name: "lo_destroy", Description: "destroy"},
		{Name: "lo_secrets_print", Description: "print"},
		{Name: "lo_init_test", Description: "scaffold"},
	}
	inj := Injection{
		Drop: []string{"lo_init_test"},
		Deny: []string{"lo_secrets_print"},
		Tiers: map[string][]string{
			"readonly":   {"lo_status"},
			"idempotent": {"lo_build"},
		},
	}
	c := newCatalog(tools, inj)

	if c.tier("lo_status") != "readonly" {
		t.Errorf("lo_status tier = %s", c.tier("lo_status"))
	}
	if c.tier("lo_build") != "idempotent" {
		t.Errorf("lo_build tier = %s", c.tier("lo_build"))
	}
	if c.tier("lo_destroy") != "mutating" {
		t.Errorf("lo_destroy tier = %s", c.tier("lo_destroy"))
	}
	if c.tag("lo_status") != "read" || c.tag("lo_build") != "read" || c.tag("lo_destroy") != "write" {
		t.Error("posture tags wrong")
	}
	if !c.isTool("lo_status") || c.isTool("nope") {
		t.Error("isTool wrong")
	}

	got := map[string]bool{}
	for _, n := range c.dieted() {
		got[n] = true
	}
	if got["lo_init_test"] {
		t.Error("dropped tool present in diet")
	}
	if got["lo_secrets_print"] {
		t.Error("denied tool present in diet")
	}
	if !got["lo_status"] || !got["lo_destroy"] {
		t.Error("diet missing expected tools")
	}

	m := c.menu()
	if !strings.Contains(m, "lo_status [read]") {
		t.Errorf("menu missing read tag:\n%s", m)
	}
	if strings.Contains(m, "second line") {
		t.Error("menu leaked second description line")
	}
	if strings.Contains(m, "lo_init_test") {
		t.Error("menu shows a dropped tool")
	}
}
