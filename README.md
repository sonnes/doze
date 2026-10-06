# doze

doze gives each dev server a `<name>.localhost` URL. You can run a server once, or register it. A registered server starts on its first request and stops when it is idle.

```sh
cd apps/web
doze pnpm dev                 # http://web.localhost while the command runs

cd ~/work/api
doze register go run ./cmd/api
curl api.localhost/health     # doze starts the api, then sends the request
```

## Install

```sh
go install github.com/sonnes/doze@latest
```

doze needs `lsof`. macOS includes it.

Then run setup once:

```sh
doze setup
```

Setup adds the doze CA to your login keychain, so that `https://<name>.localhost` works without a warning. macOS asks for your password. Setup also starts the daemon at login with launchd. To undo it, run `doze uninstall`.

To upgrade doze, install the new version. Then restart the daemon, so that it runs the new binary:

```sh
go install github.com/sonnes/doze@latest
launchctl kickstart -k gui/$(id -u)/com.github.sonnes.doze
```

If you did not run `doze setup`, run `doze daemon stop` instead of `launchctl`. The next doze command starts the new daemon.

## Run a Server Once

```sh
doze pnpm dev
doze --name docs npx serve .
doze run ls                   # use run when the command has the name of a subcommand
```

The command stays in your terminal with its output and Ctrl-C. When the command exits, the route goes away. If a registered app has the same name, doze stops it. The registered app comes back after the one-off run exits.

## Register a Server

```sh
doze register pnpm dev                     # name from package.json or the directory
doze register --name api go run ./cmd/api
doze register --idle 0 pnpm dev            # never stop when idle
doze register --name db --port 5050        # route to a server that doze does not start
```

doze saves the command, the directory, and your `PATH`. On the first request to the app, doze starts the command and holds the request until the app listens. Other requests wait for the same start.

A registered app does not get the variables from `.envrc`. If the app needs them, register the command with `direnv exec`:

```sh
doze register direnv exec . pnpm dev
```

direnv loads `.envrc` each time the app starts, so `apps.json` does not hold the values. Run `direnv allow` before the first request.

The app stops after 30 minutes without requests. An open WebSocket counts as a request, so an open Vite tab keeps its app running.

If the app exits or does not listen within 60 seconds, the proxy returns a 502 page with the last 40 lines of the log.

## Ports

Every command gets a free port in the `PORT` environment variable. Servers that ignore `PORT`, such as Vite, also work, because doze finds the port that the process group listens on.

Use `{port}` to put the port in a flag:

```sh
doze register go run . -addr :{port}
```

## A Custom Domain

Google OAuth and some other services reject `.localhost` URLs. Give the app a domain that you own:

```sh
doze setup --domain dev.example.com               # once for each domain
doze register --domain dev.example.com pnpm dev   # https://<name>.dev.example.com
```

The `<name>.localhost` URL continues to work. Setup makes a new CA that permits the domain, so macOS asks for your password again.

doze adds `<name>.<domain>` to a block in `/etc/hosts` with `sudo cp`, then runs `sudo killall -HUP mDNSResponder`. If `sudo` cannot ask for your password, run `doze hosts` in a terminal. Do not run doze with `sudo`, because a daemon that root starts runs your apps as root.

## Commands

| Command | Result |
|---|---|
| `doze <command...>` | Run the command and route `<name>.localhost` to it. |
| `doze run <command...>` | The same, for a command with the name of a subcommand. |
| `doze register <command...>` | Save the command. It starts on the first request. |
| `doze unregister <name>` | Stop the app and remove it. |
| `doze ls` | Show each app with its state, URL, and port. |
| `doze start <name>` | Start a registered app now. |
| `doze stop <name>` | Stop a registered app now. |
| `doze logs [-f] <name>` | Show the log of a registered app. |
| `doze hosts` | Write a line to `/etc/hosts` for each app with a domain. |
| `doze daemon` | Run the daemon in the foreground. |
| `doze daemon stop` | Stop the daemon and its apps. |
| `doze setup [--domain <domain>]` | Trust the HTTPS CA and start the daemon at login (macOS). `--domain` adds a domain to the CA. |
| `doze uninstall` | Remove the launchd agent, the trust of the CA, and the doze lines in `/etc/hosts`. |
| `doze version` | Show the version of doze. |

Flags go before the command: `--name`, `--port`, `--idle`, and `--domain`.

`https://localhost` shows a list of the apps.

## The Daemon

The first doze command starts the daemon in the background. The daemon runs the proxy on port 80 for HTTP and on port 443 for HTTPS. It stops all registered apps when it exits.

On macOS, a normal user can listen on `:80` and `:443` but not on `127.0.0.1:80`. The proxy listens on `:80` and `:443`. It closes each connection that does not come from the loopback address.

| Variable | Default | Use |
|---|---|---|
| `DOZE_HOME` | `~/.local/state/doze` | `apps.json`, `doze.sock`, `daemon.log`, `logs/`, and the CA |
| `DOZE_ADDR` | `:80` | The HTTP address of the proxy. Use `127.0.0.1:7355` if port 80 is not available. |
| `DOZE_HTTPS_ADDR` | `:443` | The HTTPS address of the proxy. If doze cannot listen on it, the daemon serves only HTTP. |
| `DOZE_HOSTS_FILE` | `/etc/hosts` | The hosts file that doze updates. |

When a doze command starts the daemon, the daemon gets only these environment variables: `HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `PATH`, `LANG`, `LC_*`, and `DOZE_*`. launchd gives a daemon a similar set. A registered app gets these variables, the `PATH` from its registration, and `PORT`. The app runs in the directory where you registered it.

Only your user can open the control socket and the CA key.

The URLs use HTTPS when the daemon serves it, and HTTP when it does not. If the port is not the default port, the URLs include it.

## Limits

- Chrome, Firefox, and curl send `*.localhost` to 127.0.0.1 without DNS setup. Other clients can need an `/etc/hosts` line.
- Safari and Chrome read the CA from the macOS keychain. Firefox reads it only if `security.enterprise_roots.enabled` is `true` in `about:config`.
- Node.js and other tools that do not read the keychain need the CA file, for example `NODE_EXTRA_CA_CERTS=~/.local/state/doze/ca.pem`.
- Plain HTTP on port 80 also works. Chrome and Firefox treat `http://*.localhost` as a secure context.
- A web page that you open can send a request to a registered app. This request starts the app.
- If the daemon is killed with SIGKILL, its registered apps keep running. The daemon does not find them when it starts again.

## Prior Art

doze takes its idea from [hotel](https://github.com/typicode/hotel) and [portless](https://github.com/vercel-labs/portless). Both are Node.js tools that install from npm. doze is one Go binary with no dependencies, so it needs no Node.js install.

## Development

```sh
go test -race ./...
```

The tests build a small helper server in `testdata/helper`. By default it ignores `PORT`, like Vite.

## License

MIT. See [LICENSE](LICENSE).
