"""lo chat UI — a transparent, streaming terminal assistant.

Interactive TUI (rich + prompt_toolkit) with slash commands, plus a
non-interactive `-p/--once` single-shot (rich optional) for scripting + testing.
"""
from __future__ import annotations

import sys

from lo_ai.chat.backends import load_backends
from lo_ai.chat.conductor import Conductor

try:
    from rich.console import Console
    from rich.panel import Panel
    from rich.markdown import Markdown
    from rich.live import Live
    HAVE_RICH = True
except ImportError:
    HAVE_RICH = False

SLASH = ["/help", "/quit", "/exit", "/clear", "/tools", "/model", "/posture", "/think"]


# --------------------------------------------------------------------------- #
# rendering one turn (works with a rich Console, or None for plain stdout)
# --------------------------------------------------------------------------- #
def render_turn(conductor: Conductor, msg: str, console) -> None:
    live, acc = None, ""

    def out(s, **kw):
        console.print(s, **kw) if console else print(_plain(s))

    for ev in conductor.respond(msg):
        t = ev["type"]
        if t == "route":
            out(f"[dim]→ {ev['tool']} {ev['args'] or ''}[/dim]")
        elif t == "gate":
            if ev["decision"] == "blocked":
                out(f"[bold red]⛔ {ev['tool']} blocked — {ev.get('reason', '')}[/bold red]")
            else:
                out(f"[yellow]· {ev['tool']}: {ev['decision']}[/yellow]")
        elif t == "tool":
            title = f"🔧 {ev['tool']} {ev['args'] or ''}".rstrip()
            if console:
                console.print(Panel(ev["output"] or "(no output)", title=title,
                                    border_style="cyan", expand=False))
            else:
                print(f"\n--- {title} ---\n{ev['output']}\n")
        elif t == "handoff":
            out(f"[magenta]↗ handing off to {ev['backend']}[/magenta]")
        elif t == "answer_start":
            if console and HAVE_RICH:
                live = Live(console=console, refresh_per_second=12, vertical_overflow="visible")
                live.start()
        elif t == "token":
            acc += ev["text"]
            if live:
                live.update(Markdown(acc))
            else:
                sys.stdout.write(ev["text"])
                sys.stdout.flush()
        elif t == "answer_done":
            if live:
                live.update(Markdown(acc))
                live.stop()
            elif not console:
                print()
        elif t == "error":
            out(f"[bold red]error: {ev['error']}[/bold red]")


def _plain(s: str) -> str:
    import re
    return re.sub(r"\[/?[a-z0-9 _#]+\]", "", s)


# --------------------------------------------------------------------------- #
# single-shot (-p)
# --------------------------------------------------------------------------- #
def run_once(conductor: Conductor, prompt: str) -> None:
    console = Console() if HAVE_RICH else None
    try:
        render_turn(conductor, prompt, console)
    finally:
        conductor.close()


# --------------------------------------------------------------------------- #
# interactive TUI
# --------------------------------------------------------------------------- #
def run_interactive(conductor: Conductor, backends: dict, cfg) -> None:
    if not HAVE_RICH:
        raise SystemExit("Interactive chat needs `rich` + `prompt_toolkit`:\n"
                         "  pip install -r requirements-chat.txt")
    try:
        from prompt_toolkit import PromptSession
        from prompt_toolkit.completion import WordCompleter
        from prompt_toolkit.history import InMemoryHistory
    except ImportError:
        raise SystemExit("Interactive chat needs `prompt_toolkit`:\n"
                         "  pip install -r requirements-chat.txt")

    console = Console()
    console.print(Panel.fit(
        "[bold]lo chat[/bold] — local, transparent, streaming\n"
        "[dim]/help for commands · /model to switch brain · /quit to exit[/dim]",
        border_style="green"))

    def toolbar():
        return f" {conductor.backend.label}  |  posture: {conductor.posture} "

    session = PromptSession(history=InMemoryHistory(),
                            completer=WordCompleter(SLASH, sentence=True),
                            bottom_toolbar=toolbar)

    while True:
        try:
            msg = session.prompt("\n› ").strip()
        except (EOFError, KeyboardInterrupt):
            break
        if not msg:
            continue
        if msg.startswith("/"):
            if _slash(msg, conductor, backends, console):
                break
            continue
        try:
            render_turn(conductor, msg, console)
        except KeyboardInterrupt:
            console.print("[dim](interrupted)[/dim]")
    conductor.close()
    console.print("[dim]bye.[/dim]")


def _slash(msg: str, conductor: Conductor, backends: dict, console) -> bool:
    """Handle a slash command. Returns True to quit."""
    parts = msg.split()
    cmd, arg = parts[0], (parts[1] if len(parts) > 1 else "")
    if cmd in ("/quit", "/exit"):
        return True
    elif cmd == "/help":
        console.print(Panel(
            "/model [name]   list backends / switch the conductor (local or frontier CLI)\n"
            "/posture [mode] show or set: read-only | confirm | open\n"
            "/think on|off   toggle reasoning (if the model supports it)\n"
            "/tools          list the tools the conductor can use\n"
            "/clear          reset the conversation\n"
            "/quit           exit", title="commands", border_style="green"))
    elif cmd == "/tools":
        rows = "\n".join(f"  {n} [{conductor._tag(n)}]" for n in conductor.tools)
        console.print(Panel(rows, title=f"{len(conductor.tools)} tools", border_style="cyan"))
    elif cmd == "/model":
        if not arg:
            lines = []
            for name, b in backends.items():
                mark = "●" if b is conductor.backend else ("○" if b.available() else "[dim]✗ not installed[/dim]")
                lines.append(f"  {mark} {name}: {b.label}")
            console.print(Panel("\n".join(lines), title="backends (/model <name>)", border_style="green"))
        elif arg in backends:
            b = backends[arg]
            if not b.available():
                console.print(f"[red]{arg} is not installed (need `{getattr(b, 'detect', arg)}` on PATH)[/red]")
            else:
                conductor.set_backend(b)
                console.print(f"[green]switched to {b.label}[/green]")
        else:
            console.print(f"[red]unknown backend '{arg}'. /model to list.[/red]")
    elif cmd == "/posture":
        if arg in ("read-only", "confirm", "open"):
            conductor.posture = arg
            console.print(f"[green]posture: {arg}[/green]")
        else:
            console.print(f"posture: {conductor.posture}  (set: read-only | confirm | open)")
    elif cmd == "/think":
        b = conductor.backend
        if hasattr(b, "think"):
            b.think = (arg == "on")
            console.print(f"[green]think: {b.think}[/green]")
        else:
            console.print("[yellow]current backend has no think toggle[/yellow]")
    elif cmd == "/clear":
        conductor.history.clear()
        console.print("[dim]conversation cleared[/dim]")
    else:
        console.print(f"[red]unknown command {cmd} (/help)[/red]")
    return False


def run(cfg, prompt: str = None, model: str = None, posture: str = None) -> None:
    backends = load_backends(cfg)
    if not backends:
        raise SystemExit("no chat backends configured (see chat.backends in config)")
    name = model or cfg.get("chat.conductor") or next(iter(backends))
    if name not in backends:
        raise SystemExit(f"unknown conductor '{name}'. configured: {', '.join(backends)}")
    conductor = Conductor(cfg, backends[name], posture)
    if prompt:
        run_once(conductor, prompt)
    else:
        run_interactive(conductor, backends, cfg)
