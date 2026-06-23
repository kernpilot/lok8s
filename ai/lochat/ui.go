package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
	cMag    = "\033[35m"
)

func argstr(a map[string]any) string {
	if len(a) == 0 {
		return ""
	}
	b, _ := json.Marshal(a)
	return string(b)
}

func panel(title, body string) {
	fmt.Printf("%s┌─ %s%s%s\n", cCyan, cBold, title, cReset)
	for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if len([]rune(l)) > 78 {
			l = string([]rune(l)[:77]) + "…"
		}
		fmt.Printf("%s│%s %s\n", cCyan, cReset, l)
	}
	fmt.Printf("%s└─%s\n", cCyan, cReset)
}

func renderTurn(c *Conductor, msg string) {
	answering := false
	for e := range c.Respond(msg) {
		switch e.Type {
		case "route":
			fmt.Printf("%s→ %s %s%s\n", cDim, e.Tool, argstr(e.Args), cReset)
		case "gate":
			if e.Decision == "blocked" {
				fmt.Printf("%s%s⛔ %s blocked — %s%s\n", cBold, cRed, e.Tool, e.Reason, cReset)
			} else {
				fmt.Printf("%s· %s: %s%s\n", cYellow, e.Tool, e.Decision, cReset)
			}
		case "tool":
			panel(strings.TrimSpace("🔧 "+e.Tool+" "+argstr(e.Args)), e.Output)
		case "handoff":
			fmt.Printf("%s↗ handing off to %s%s\n", cMag, e.Backend, cReset)
		case "answer_start":
			fmt.Println()
			answering = true
		case "token":
			fmt.Print(e.Text)
		case "answer_done":
			if answering {
				fmt.Println()
			}
		case "error":
			fmt.Printf("%serror: %s%s\n", cRed, e.Error, cReset)
		}
	}
}

func interactive(c *Conductor, backends map[string]Backend) {
	fmt.Printf("%s%slo chat%s %s— local · transparent · streaming · /help · /quit%s\n",
		cBold, cGreen, cReset, cDim, cReset)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 8<<20)
	for {
		fmt.Printf("\n%s[%s · %s]%s\n› ", cDim, c.backend.Label(), c.posture, cReset)
		if !in.Scan() {
			break
		}
		msg := strings.TrimSpace(in.Text())
		if msg == "" {
			continue
		}
		if strings.HasPrefix(msg, "/") {
			if slash(c, backends, msg) {
				break
			}
			continue
		}
		renderTurn(c, msg)
	}
	fmt.Printf("\n%sbye.%s\n", cDim, cReset)
}

func slash(c *Conductor, backends map[string]Backend, msg string) bool {
	f := strings.Fields(msg)
	cmd, arg := f[0], ""
	if len(f) > 1 {
		arg = f[1]
	}
	switch cmd {
	case "/quit", "/exit":
		return true
	case "/help":
		fmt.Println(`  /model [name]   list/switch the conductor (local or frontier CLI)
  /posture [mode] read-only | confirm | open
  /think on|off   toggle reasoning (http backends)
  /tools          list available tools
  /clear          reset the conversation
  /quit           exit`)
	case "/tools":
		for _, n := range c.catalog.dieted() {
			fmt.Printf("  %s [%s]\n", n, c.catalog.tag(n))
		}
	case "/model":
		if arg == "" {
			for name, b := range backends {
				mark := "○"
				if b == c.backend {
					mark = cGreen + "●" + cReset
				} else if !b.Available() {
					mark = cDim + "✗" + cReset
				}
				fmt.Printf("  %s %s: %s\n", mark, name, b.Label())
			}
		} else if b, ok := backends[arg]; ok {
			if !b.Available() {
				fmt.Printf("%s%s not installed%s\n", cRed, arg, cReset)
			} else {
				c.setBackend(b)
				fmt.Printf("%sswitched to %s%s\n", cGreen, b.Label(), cReset)
			}
		} else {
			fmt.Printf("%sunknown backend %q%s\n", cRed, arg, cReset)
		}
	case "/posture":
		if arg == "read-only" || arg == "confirm" || arg == "open" {
			c.posture = arg
			fmt.Printf("%sposture: %s%s\n", cGreen, arg, cReset)
		} else {
			fmt.Printf("posture: %s  (read-only|confirm|open)\n", c.posture)
		}
	case "/think":
		if hb, ok := c.backend.(*httpBackend); ok {
			v := arg == "on"
			hb.cfg.Think = &v
			fmt.Printf("%sthink: %v%s\n", cGreen, v, cReset)
		} else {
			fmt.Printf("%sbackend has no think toggle%s\n", cYellow, cReset)
		}
	case "/clear":
		c.history = nil
		fmt.Printf("%scleared%s\n", cDim, cReset)
	default:
		fmt.Printf("%sunknown %s (/help)%s\n", cRed, cmd, cReset)
	}
	return false
}
