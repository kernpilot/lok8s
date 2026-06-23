package main

import "strings"

type Tool struct {
	Name        string
	Description string
}

type Catalog struct {
	tools                            map[string]*Tool
	order                            []string
	readonly, idempotent, deny, drop map[string]bool
}

func toSet(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func newCatalog(ts []Tool, inj Injection) *Catalog {
	c := &Catalog{
		tools:      map[string]*Tool{},
		readonly:   toSet(inj.Tiers["readonly"]),
		idempotent: toSet(inj.Tiers["idempotent"]),
		deny:       toSet(inj.Deny),
		drop:       toSet(inj.Drop),
	}
	for i := range ts {
		t := ts[i]
		c.tools[t.Name] = &t
		c.order = append(c.order, t.Name)
	}
	return c
}

func (c *Catalog) tier(n string) string {
	switch {
	case c.readonly[n]:
		return "readonly"
	case c.idempotent[n]:
		return "idempotent"
	default:
		return "mutating"
	}
}

// tag: the posture label shown to the model — read vs write.
func (c *Catalog) tag(n string) string {
	if c.tier(n) == "mutating" {
		return "write"
	}
	return "read"
}

func (c *Catalog) isTool(n string) bool { _, ok := c.tools[n]; return ok }

// exposed: is this tool actually on the chat surface? drop/deny-filtered tools
// (plumbing + secret-readers) are off the menu and must NEVER run, even if the
// model names one directly and it happens to be read-tagged.
func (c *Catalog) exposed(n string) bool {
	return c.isTool(n) && !c.deny[n] && !c.drop[n]
}

// dieted: the chat surface — minus plumbing (drop) and secret-readers (deny).
func (c *Catalog) dieted() []string {
	var out []string
	for _, n := range c.order {
		if c.deny[n] || c.drop[n] {
			continue
		}
		out = append(out, n)
	}
	return out
}

func (c *Catalog) menu() string {
	var b strings.Builder
	for _, n := range c.dieted() {
		d := c.tools[n].Description
		if i := strings.IndexByte(d, '\n'); i >= 0 {
			d = d[:i]
		}
		b.WriteString("- " + n + " [" + c.tag(n) + "]: " + d + "\n")
	}
	return b.String()
}
