# Connect an AI agent

`lo mcp` is the MCP server of lok8s. It serves the `lo` commands as tools
over the [Model Context Protocol](https://modelcontextprotocol.io/) (MCP). An
AI agent in your editor can then run `lo status`, `lo lint` or `lo audit` and
read the output. These three are in the default `readonly` tier. A command
that changes something, such as `lo build`, needs a higher tier.

## What the server offers

- Each leaf command (a command without subcommands) of the `lo` command tree
  becomes one tool. The tool name is `lo_` plus the command path with
  underscores: `lo_status`, `lo_secrets_encrypt`, `lo_registry_up`.
- A tool call runs the same `lo` binary as a subprocess. The agent gets the
  stdout, the stderr and the exit code.
- Three tiers set which tools the agent can see:

| Tier | Flag | Environment form | Adds |
|------|------|------------------|------|
| `readonly` (default) | none | none | `status`, `lint`, `audit`, `secrets list`, `tilt status`, … |
| `mutating` | `--allow-mutating` | `LO_MCP_ALLOW=mutating` | `build`, `use`, `init`, `secrets encrypt`, … |
| `destructive` | `--allow-destructive` | `LO_MCP_ALLOW=destructive` | `up`, `down`, `deploy`, `destroy`, … and the `--force` flags |

The server does not register a tool outside its tier, so the agent cannot
call it. These commands are never tools, in any tier:

- The Go-only top-level commands: `lo toolchain`, `lo assets`, `lo mcp` and
  `lo operator`.
- A command whose output is a credential: `kubeconfig`, `secrets print`,
  `secrets env`, …
- `lo chat`, because it passes its arguments on unread.
- The hidden, framework-internal commands.

A flag that carries a credential is never a tool parameter.

To see the tools of a tier, run `lo mcp tools`:

```bash
lo mcp tools                    # the readonly tier
lo mcp tools --allow-mutating   # readonly + mutating
lo mcp tools --json             # full definitions with the input schemas
```

## Register the server with an editor

`lo mcp <editor> enable` writes a server entry into the MCP config of the
editor. Three editors are supported:

| Command | Editor |
|---------|--------|
| `lo mcp claude …` | Claude Desktop |
| `lo mcp vscode …` | VS Code (Copilot agent mode) |
| `lo mcp cursor …` | Cursor |

1. Go to the project directory. The entry records this directory as the
   project (`PATH_BASE`).
2. Write the entry:

   ```bash
   lo mcp vscode enable
   ```

3. Restart the editor, or reload its MCP servers.

The entry starts `<absolute path of lo> mcp start`. It also sets `PATH`
(with the `.bin` toolchain first) and `PATH_BASE`. An editor starts the
server with a small environment and another working directory, so the
server needs both.

The entry opens the `readonly` tier. To open a higher tier, add the
environment form:

```bash
lo mcp vscode enable --env LO_MCP_ALLOW=mutating
```

`enable` takes these flags:

| Flag | Effect |
|------|--------|
| `--env KEY=value` | Adds a variable to the entry. Repeat it for more variables. |
| `--server-name <name>` | Names the entry. The default is the name of the executable (`lo`). |
| `--log-level <level>` | Sets the server log level: `debug`, `info`, `warn` or `error`. |
| `--workspace` | `vscode` and `cursor` only: writes `.vscode/mcp.json` or `.cursor/mcp.json` in the current directory, not the user config. |
| `--config-path <file>` | Writes this config file. |

Without `--workspace` or `--config-path`, `enable` writes the user config:

| Editor | Linux | macOS |
|--------|-------|-------|
| `claude` | `~/.config/Claude/claude_desktop_config.json` (`$XDG_CONFIG_HOME` first) | `~/Library/Application Support/Claude/claude_desktop_config.json` |
| `vscode` | `~/.config/Code/User/mcp.json` | `~/Library/Application Support/Code/User/mcp.json` |
| `cursor` | `~/.cursor/mcp.json` | `~/.cursor/mcp.json` |

## List and remove entries

```bash
lo mcp vscode list      # show the MCP servers in the VS Code config
lo mcp vscode disable   # remove the lo entry
```

`list` and `disable` take `--config-path`. For `vscode` and `cursor`, they
also take `--workspace`. If you named the entry with `--server-name`, give
the same name to `disable`.

## Start the server yourself

An agent that reads its own MCP config can start the server directly:

- `lo mcp start` serves stdio. This is what editors launch.
- `lo mcp serve` serves streamable HTTP on `127.0.0.1:8080` (`--host`,
  `--port`). It has no authentication, so keep it on loopback.

Both take `--allow-mutating` and `--allow-destructive`. `LO_MCP_ALLOW` adds
to these flags: the higher of the two tiers applies. The [CLI reference](/reference/cli#lo-mcp) shows a
`.mcp.json` entry and lists the tool counts of each tier.
