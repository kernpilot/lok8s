package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// ANSI styles — vars (not consts) so they can be blanked when stdout isn't a
// TTY (piped/redirected) or NO_COLOR is set. That keeps `lo chat -p … > file`
// clean: plain text + raw markdown, no escape codes.
var (
	cReset, cDim, cBold, cUnder, cItal string
	cRed, cGreen, cYellow, cCyan, cMag string
	useColor                           = colorEnabled()
)

func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("LOCHAT_FORCE_COLOR") != "" {
		return true
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// stderrTTY reports whether stderr is an interactive terminal (so transient
// status hints don't emit cursor-control escapes into a redirected stream).
func stderrTTY() bool {
	fi, err := os.Stderr.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// stdinTTY reports whether stdin is interactive — so we only prompt (y/N) when
// a human can answer; piped/CI runs just get the command printed.
func stdinTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func init() {
	if !useColor {
		return
	}
	cReset, cDim, cBold, cUnder, cItal = "\033[0m", "\033[2m", "\033[1m", "\033[4m", "\033[3m"
	cRed, cGreen, cYellow, cCyan, cMag = "\033[31m", "\033[32m", "\033[33m", "\033[36m", "\033[35m"
}

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

// ---- streaming markdown ----

// mdStream renders the model's streamed answer as light markdown. Tokens arrive
// piecemeal, so we buffer to the next newline and render whole lines (inline
// styles are line-scoped; fenced code blocks are tracked across lines). Output
// stays live at line granularity.
type mdStream struct {
	w       io.Writer
	buf     string
	inFence bool
}

func (m *mdStream) feed(tok string) {
	m.buf += tok
	for {
		i := strings.IndexByte(m.buf, '\n')
		if i < 0 {
			return
		}
		fmt.Fprintln(m.w, renderLine(m.buf[:i], &m.inFence))
		m.buf = m.buf[i+1:]
	}
}

func (m *mdStream) flush() {
	if m.buf != "" {
		fmt.Fprintln(m.w, renderLine(m.buf, &m.inFence))
		m.buf = ""
	}
}

func renderLine(line string, inFence *bool) string {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "```") {
		*inFence = !*inFence
		if lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```")); *inFence && lang != "" {
			return cDim + "┄┄┄ " + lang + " ┄┄┄" + cReset
		}
		return cDim + "┄┄┄┄┄┄" + cReset
	}
	if *inFence {
		return cCyan + line + cReset
	}
	if trimmed == "" {
		return ""
	}
	if n := headerLevel(trimmed); n > 0 {
		return cBold + cUnder + strings.TrimSpace(trimmed[n:]) + cReset
	}
	if strings.HasPrefix(trimmed, "> ") {
		return cDim + "▏ " + renderInline(strings.TrimPrefix(trimmed, "> ")) + cReset
	}
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	rest := line[len(indent):]
	if len(rest) >= 2 && (rest[0] == '-' || rest[0] == '*' || rest[0] == '+') && rest[1] == ' ' {
		return indent + cYellow + "•" + cReset + " " + renderInline(rest[2:])
	}
	if num, content := numbered(rest); num != "" {
		return indent + cYellow + num + cReset + " " + renderInline(content)
	}
	return renderInline(line)
}

func headerLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n
}

func numbered(s string) (string, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(s) && (s[i] == '.' || s[i] == ')') && s[i+1] == ' ' {
		return s[:i+1], s[i+2:]
	}
	return "", ""
}

// renderInline applies `code`, **bold**, and *italic* within one line. Order
// matters: code is consumed first so its contents aren't re-parsed; single-`*`
// italic guards against spaces (so "2 * 3" stays literal) and `_` is left alone
// (snake_case identifiers are everywhere in cluster ops).
func renderInline(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		switch {
		case rs[i] == '`':
			if j := idxRune(rs, '`', i+1); j > i {
				b.WriteString(cCyan + string(rs[i+1:j]) + cReset)
				i = j + 1
				continue
			}
		case rs[i] == '*' && i+1 < len(rs) && rs[i+1] == '*':
			if j := idxBold(rs, i+2); j >= 0 {
				b.WriteString(cBold + string(rs[i+2:j]) + cReset)
				i = j + 2
				continue
			}
		case rs[i] == '*':
			if i+1 < len(rs) && rs[i+1] != ' ' {
				if j := idxRune(rs, '*', i+1); j > i+1 && rs[j-1] != ' ' {
					b.WriteString(cItal + string(rs[i+1:j]) + cReset)
					i = j + 1
					continue
				}
			}
		}
		b.WriteRune(rs[i])
		i++
	}
	return b.String()
}

func idxRune(rs []rune, r rune, from int) int {
	for i := from; i < len(rs); i++ {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

func idxBold(rs []rune, from int) int {
	for i := from; i+1 < len(rs); i++ {
		if rs[i] == '*' && rs[i+1] == '*' {
			return i
		}
	}
	return -1
}

// ---- turn rendering ----

func renderTurn(c *Conductor, msg string) {
	answering := false
	md := &mdStream{w: os.Stdout}
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
			if useColor {
				md.feed(e.Text) // render markdown for terminals
			} else {
				fmt.Print(e.Text) // piped: keep raw markdown intact
			}
		case "answer_done":
			if useColor {
				md.flush()
			}
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

// ---- slash commands (with short aliases + cycling shortcuts) ----

var postures = []string{"read-only", "open"}

func nextPosture(cur string) string {
	for i, p := range postures {
		if p == cur {
			return postures[(i+1)%len(postures)]
		}
	}
	return postures[0]
}

func sortedNames(backends map[string]Backend) []string {
	ns := make([]string, 0, len(backends))
	for n := range backends {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

// cycleBackend advances to the next *available* backend (wraps; stays put if
// the current is the only one installed).
func cycleBackend(c *Conductor, backends map[string]Backend) {
	names := sortedNames(backends)
	cur := 0
	for i, n := range names {
		if backends[n] == c.backend {
			cur = i
		}
	}
	for step := 1; step <= len(names); step++ {
		if cand := backends[names[(cur+step)%len(names)]]; cand.Available() {
			c.setBackend(cand)
			return
		}
	}
}

func listModels(c *Conductor, backends map[string]Backend) {
	for _, n := range sortedNames(backends) {
		b := backends[n]
		mark := "○"
		if b == c.backend {
			mark = cGreen + "●" + cReset
		} else if !b.Available() {
			mark = cDim + "✗" + cReset
		}
		fmt.Printf("  %s %s: %s\n", mark, n, b.Label())
	}
}

func slash(c *Conductor, backends map[string]Backend, msg string) bool {
	f := strings.Fields(msg)
	cmd, arg := f[0], ""
	if len(f) > 1 {
		arg = f[1]
	}
	switch cmd {
	case "/quit", "/exit", "/q":
		return true
	case "/help", "/?", "/h":
		fmt.Println(`  /model  /m [name]    no arg: cycle to next model · name: switch
  /models              list models (● active · ✗ not installed)
  /posture /p [mode]   no arg: cycle · or read-only | open
  /think  /t [on|off]  toggle reasoning (http backends)
  /tools               list available tools
  /clear  /c           reset the conversation
  /help   /?           this help
  /quit   /q           exit`)
	case "/tools":
		for _, n := range c.catalog.dieted() {
			fmt.Printf("  %s [%s]\n", n, c.catalog.tag(n))
		}
	case "/models":
		listModels(c, backends)
	case "/model", "/m":
		switch {
		case arg == "":
			cycleBackend(c, backends)
			fmt.Printf("%smodel: %s%s\n", cGreen, c.backend.Label(), cReset)
		default:
			if b, ok := backends[arg]; !ok {
				fmt.Printf("%sunknown backend %q (/models)%s\n", cRed, arg, cReset)
			} else if !b.Available() {
				fmt.Printf("%s%s not installed%s\n", cRed, arg, cReset)
			} else {
				c.setBackend(b)
				fmt.Printf("%smodel: %s%s\n", cGreen, b.Label(), cReset)
			}
		}
	case "/posture", "/p":
		switch arg {
		case "":
			c.posture = nextPosture(c.posture)
			fmt.Printf("%sposture: %s%s\n", cGreen, c.posture, cReset)
		case "read-only", "open":
			c.posture = arg
			fmt.Printf("%sposture: %s%s\n", cGreen, arg, cReset)
		default:
			fmt.Printf("posture: %s  (read-only|open)\n", c.posture)
		}
	case "/think", "/t":
		hb, ok := c.backend.(*httpBackend)
		if !ok {
			fmt.Printf("%sbackend has no think toggle%s\n", cYellow, cReset)
			break
		}
		var v bool
		switch arg {
		case "on":
			v = true
		case "off":
			v = false
		default:
			v = !(hb.cfg.Think != nil && *hb.cfg.Think) // toggle current
		}
		hb.cfg.Think = &v
		fmt.Printf("%sthink: %v%s\n", cGreen, v, cReset)
	case "/clear", "/c":
		c.history = nil
		fmt.Printf("%scleared%s\n", cDim, cReset)
	default:
		fmt.Printf("%sunknown %s (/help)%s\n", cRed, cmd, cReset)
	}
	return false
}
