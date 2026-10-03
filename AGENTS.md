# doze

doze gives each dev server a `<name>.localhost` URL. A one-off run (`doze pnpm dev`) routes until the command exits. A registered app starts on its first request and stops when it is idle. README.md is the user guide.

doze is one `main` package at the repository root. It uses Go and the standard library only. Do not add a dependency. macOS is the main target, and doze uses `lsof` to find ports.

- `cli.go` - argument parsing and one-off runs
- `setup.go` - `doze setup` and `doze uninstall`: the keychain trust and the launchd agent
- `client.go` - the client for the control socket
- `daemon.go`, `control.go`, `proxy.go` - the route table, lazy start, idle stop, the proxy, and the control protocol
- `proc.go` - process groups, port discovery, and stop
- `tls.go` - the local CA and a certificate for each host
- `testdata/helper/` - the helper dev server for tests

## Commands

Before you commit, run the first three commands. The output of `gofmt -l .` must be empty.

```sh
go test -race ./...
go vet ./...
gofmt -l .
go build -o /tmp/doze .
```

## Code

- Write vertical code. Return early. Put each argument of a long call and each field of a struct literal on its own line.
- If a choice has a reason, write the reason in the doc comment.

## Tests

Write the test first. Run it and make sure that it fails for the right reason. Then write the minimum code that makes it pass.

- Use the standard `testing` package. Do not add an assertion library.
- Test behavior through requests to the proxy, calls to the client, and runs of the CLI.
- For a dev server, use `helperPath(t)`. The helper ignores `PORT`, like Vite.
- For a directory that holds a Unix socket, use `shortTempDir(t)`. On macOS, a socket path must be less than 104 bytes.
- Do not use fixed sleeps. Poll with a deadline, as `eventually` in `daemon_test.go` does.
- Do not run `doze setup` or `doze uninstall` in a test or a smoke check. They change the login keychain and the launchd agents of the user.

## Rules

- `Process.Stop` waits until every process in the group exits. Do not call it while you hold `Daemon.mu`. Use `stopLocked` under the lock. Then call `stopProc` after you unlock.
- Each start and each stop increments `route.gen`. A goroutine or a timer that captured an old `gen` must do nothing.
- The control protocol is one JSON line per request and one per response. An `attach` connection stays open, and the one-off route lasts until it closes.
- The proxy keeps the original `Host` header, because Vite and other dev servers examine it.
- The proxy listens on `:80` and `:443`, because macOS does not let a normal user listen on `127.0.0.1:80`. Keep `LoopbackOnly` on both listeners.
- `apps.json` must not hold secrets. A registration saves only `PATH` from the environment.
- If you change a command, a flag, or a behavior that users see, update README.md and the usage text in `cli.go`.
