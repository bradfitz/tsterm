# tsterm notes

Working notes: where things stand, what's next, and what's being explored.
Keep this current as work lands.

## What tsterm is

A Tailscale-native replacement for Mosh. The server, tstermd, owns the
canonical terminal state: it parses each session's output with its own
terminal emulator and sends clients screen updates, not raw bytes. Clients
(the `tsterm` CLI, the web UI, future GUI apps) render that state. Tailscale
(WireGuard) provides encryption and peer identity, so there is no SSH and no
application-layer crypto. The original design discussion is in ~/tsterm.pdf.

## Status

Working, and deployed on braid as a systemd user unit (`tstermd`):

- **Processes.** `tstermd` serves the tailnet. Each session's PTY and child
  live in a shim process, tstermd's own binary run again (selected by
  `TSTERM_SHIM_SPAWN`; see `shim.MainIfRequested`; named `tstermd-shim` in
  ps), one per session, under systemd in its own transient unit
  `tsterm-pty-<ID>`, so tstermd can restart or upgrade without killing
  sessions. tstermd runs shims from `/proc/self/exe`, so they're the same
  build as the tstermd that starts them, even after its file is replaced.
  The tstermd/shim protocol is `pkg/shimproto`; a new tstermd must still
  speak it to shims from older builds, which run until their sessions end.
- **Restarts.** tstermd checkpoints each session's terminal state to its
  shim, restores sessions on startup, and pushes the screen to the sessions'
  last-known UDP clients, so idle clients catch up unprompted.
- **UDP hot path** (`pkg/transport/udp`). Mosh-style state sync: numbered
  states, diffs from the client's acknowledged state, zstd-compressed and
  fragmented to fit the tailnet MTU. No keepalives: an idle session sends
  nothing in either direction (for mobile battery). Retries back off and give
  up after 60s; undelivered keystrokes are dropped rather than replayed late.
- **Control plane** (`pkg/transport/tcp`): HTTP, CBOR or JSON. Create, list
  (sortable by activity or attach time), rename, and kill sessions.
- **Auth** (`pkg/auth`): only the node owner's tailnet user; peer identities
  are tracked from the IPN bus. Listens on all Tailscale addresses (IPv4 and
  IPv6).
- **CLI client** (`cmd/tsterm`): attach, list, create, kill; status line when
  the server is unreachable; Ctrl-^ . disconnects; mirrors mouse, bracketed
  paste, cursor-key, and focus modes onto the local terminal.
- **Web UI** (`http://HOST:2200/`, owner only): session list (names first,
  linking to the sessions, unnamed ones as their IDs' dim starts, with a
  pencil to rename on hovering over a row) with renaming
  and creation; per-session viewer with typing (on by default), view-only
  mode, auto-fit (size-owning) mode, mouse input, and tabs of recently
  active sessions in the one header bar (scrolling sideways, by wheel too,
  when they don't fit; double-click the current one to rename it). The
  cursor follows the program's shape and blink, like Ghostty: an inverted
  block by default, hollow while the page isn't focused. Tabs switch
  sessions in place (a new WebSocket, `history.pushState`; back and forward
  work), without reloading the page. Auto-fit is one preference for the
  whole web app, shared by its windows through localStorage, and applied
  by writable viewers. Clients that want different sizes don't fight: each
  has a `session.Sizer` recording the size it wants, and only the leader's
  applies, the client the user used last (typed into, focused, resized
  while in front, attached, or turned auto-fit on in). Viewers learn
  whether they lead from `proto.Sizing` frames and say who does. It fills
  the whole terminal area; its toggle, and a
  one-time fit when the terminal is too big to see whole, pop up near the
  area's bottom-right corner (on touch devices, a key in the extra-keys bar);
  when the terminal is smaller than the area, a ⇲ toggle also sits in the
  checkerboard at the terminal's own corner. Mobile friendly: typing goes through a hidden text
  field, which is how the mobile keyboard appears (a ⌨ button shows and
  hides it) and how IME and autocorrect composition arrive (sent live, as
  diffs); touch devices get an extra-keys bar (Esc, Tab, sticky Ctrl and
  Alt, arrows, | ~ / -); the page fits the visual viewport, so size-owning
  mode fits the space above the keyboard.
- **HTTPS** on the same port as HTTP (connections are sniffed: TLS starts
  with 0x16), with the node's certificate from tailscaled, when the tailnet
  has HTTPS certificates enabled (the node's MagicDNS name is in the
  Status's `CertDomains`). Cleartext requests for web pages redirect to
  `https://<MagicDNS name>:<port>/...`, since browsers allow notifications
  and other features only on secure pages; the API stays on cleartext (it's
  inside WireGuard) for the CLI. Live on braid at
  https://braid.corp.ts.net:2200/.
- **Sessions**: IDs are `pty-` + `crypto/rand.Text()`; optional names (see
  "Naming"). Auto-naming (`session.Manager.AutoName`, every 5 s, Linux
  only, via /proc) names a session that has never had a name for the
  program in front, its terminal's foreground process group's leader: the
  worktree on a `claude -w NAME` (or `--worktree`) command line, or else the
  linked git worktree it's working in; taken names get `-2` and up. A
  session that has had a name, from the user or auto-naming (`Named` in its
  metadata), is left be, even if the name is removed. TODO: macOS (libproc);
  maybe more rules (ssh hosts, tmux session names). Every session's TERM is xterm-256color (tstermd's emulator),
  and LANG defaults to C.UTF-8.

Run it: `go install github.com/bradfitz/tsterm/cmd/tstermd@latest` (or
`./cmd/...` from a checkout), then `tstermd install`, which checks the
prerequisites (systemd's user manager, lingering, which it enables if it
can, Tailscale up, the operator setting and HTTPS certificates, warning with
the fix), writes `~/.config/systemd/user/tstermd.service` (`ExecStart=...
tstermd run [the flags given]`), replaces any transient unit of that name,
enables and restarts it, and prints the URL. Upgrading is the same: go
install, then `tstermd install` again (or `systemctl --user restart
tstermd`); sessions survive. `tstermd uninstall` stops and removes it,
leaving sessions running and their data. Plain `tstermd` prints this, and
on macOS says launchd support is a TODO. `tstermd run` runs in the
foreground. No README yet, by choice: it's public, but not announced.

## Terminal emulator: libghostty-vt

The emulator (`pkg/vt`) is libghostty-vt, Ghostty's, compiled to wasm32 and
run with wazero, so no cgo. It replaced charmbracelet/x/vt on 2026-11-11.

- **Module.** The wasm lives in its own repo and Go module,
  `github.com/bradfitz/tsterm-prebuilt/ghosttyvt` (local checkout:
  ~/src/github.com/bradfitz/tsterm-prebuilt), which exports it as
  `ghosttyvt.Wasm` so tsterm's history doesn't grow with each rebuild. Its
  `build.sh` fetches a pinned Zig (0.17.1) and the `vt-1.0.0` source, builds
  `libghostty-vt.a` for wasm32, links our glue (`glue/tsterm.c`) into
  `ghosttyvt.wasm` (~890 KB; reproducible), and generates `version.go`. The
  glue's contract with `pkg/vt/ghostty.go` (exports, imports, snapshot
  encoding) is versioned by `TSTERM_ABI_VERSION`, checked at load. To change
  the glue: edit and rebuild it there, develop against it with a temporary
  `replace` in go.mod, and once its commit is pushed, `go get
  github.com/bradfitz/tsterm-prebuilt/ghosttyvt@<commit>` and drop the
  `replace`, so that every tsterm commit builds on its own. Licenses:
  Ghostty's MIT `LICENSE`, and `THIRD_PARTY_LICENSES` for the other linked
  code (uucode, translate_c, Zig's compiler_rt). The stock wasm build can't
  be used: it has no imports and delivers effects (replies to terminal
  queries, notifications) via C function pointers, which a wasm host can't
  supply; the glue forwards them to host imports, and encodes a whole screen
  in one call so the host crosses the wasm boundary once per snapshot.
  To update it: change and rebuild in tsterm-prebuilt, push, then run
  `go get github.com/bradfitz/tsterm-prebuilt/ghosttyvt@main` here.
- **Checkpoints** hold libghostty's native snapshot (both screens, scroll
  region, saved cursor, pen, pending wrap, unfinished escape sequences,
  scrollback) plus our ANSI repaint as a fallback, because libghostty
  versions its snapshot format separately from its ABI. Checkpoints from
  before the switch (bare repaints) still restore.
- **Demo and tests.** `internal/demoapp` is a full-screen program showing
  off what a terminal can do: colors, styles, Unicode, a hyperlink, title
  animation, cursor styles, bell, notifications, mouse clicks, paste, focus,
  resizing, and replies to queries. Sessions named `_tsterm_demo` run it
  (tstermd re-executes itself as the demo); start one with
  `tsterm -new HOST _tsterm_demo`, the index page's "Try the demo", or
  shift-click on "+" in the viewer's tabs. Names starting with `_tsterm`
  are reserved. Tests run it too, as a stand-in for programs such as
  editors (test binaries run themselves in a PTY); see
  `pkg/session/demoapp_test.go`.
- **Desktop notifications** (OSC 9 and 777) travel in the screen state, like
  the bell. The CLI passes them to the local terminal (control characters
  stripped). The web viewer shows them as toasts while the page is in
  front, and with the browser's Notification API otherwise (once allowed;
  needs HTTPS; they renotify, replacing the session's previous one, and any
  the system couldn't show become toasts when the page is back). Being state, notifications sent within one screen update
  coalesce to the last. The demo's `N` key notifies after 5 seconds, to try
  the unfocused path. The viewer's bell beeps (Web Audio, unlocked by the
  first key press or click) and flashes the screen.
- **History.** Lines that scroll off the primary screen go to disk, in
  `pkg/history`: zstd-compressed CBOR frames of text-plus-style-run lines
  in ~4 MB segment files under `-sessions-dir`
  (`~/.local/state/tsterm/sessions/<id>/history`), 256 MB per session by
  default, oldest segments dropped first. The glue tracks the first row not
  yet archived with a tracked grid ref (it follows scrolling, pruning, and
  reflow) and formats new lines with libghostty's formatter (VT text,
  unwrapped), which Go parses into `vt.Line`s; checkpoints carry the mark
  and the stored line count, so a restart's replay doesn't duplicate lines.
  Each session also saves its screen (`<id>/screen`, zstd CBOR of a
  `session.SavedScreen`) every 5 s while it changes and when it ends,
  after syncing its history, atomically (temp file, fsync, rename), so
  that after a crash of anything, even the system, what was on the screen
  and above it survives. Screens carry the line count (`h`); WebSocket viewers ask for ranges
  (`{"history": {"from", "count"}}`). The web viewer shows history above the
  screen, loads older lines near the top, stays pinned to the bottom, and
  hides it on the alternate screen. Cost: writes of `cat`-like output drop
  from ~260 to ~55 MB/s, mixed output from ~144 to ~100 MB/s (formatting
  plus parsing; per-cell glue calls were far slower).
- **Three properties of a session**, independent: *alive* or frozen (ended);
  *open* or closed, its membership in the user's open set, the sessions
  with tabs, shared by every device; and *stored*, which retention decides
  (live sessions always; ended ones for a year, the oldest going first
  past 10 GiB in total), or an explicit purge. Closing a tab only takes it
  out of the open set: the session runs on, and the session list
  (Running, with a reopen button) and the tab bar's "Other (N)", which
  counts closed sessions still running, find it again. A closing tab
  flies into Other, whose count goes up, which shows where it went without
  any explaining. Visiting a session's page, or attaching with the CLI,
  reopens it. Tabs are in the order opened (`opened`, kept in the shim's
  metadata and the session's `info`). PATCH `{"closed": bool}` works on
  live and ended sessions; DELETE ends a live one, and only `?purge=1`
  deletes what an ended one kept (the session list's "delete forever",
  after a confirmation). Each session's `info` keeps its name, command,
  creation and end times, how it ended, and its open state.
- **Frozen sessions.** A session that ends (its program exits, or it's
  killed) stays, frozen: listed with when and how it ended (`ended`,
  `status` in the API), and viewable read-only on the web, its last screen
  with its history above. Each session's directory has an `info` file
  (name, times, end), its `screen`, and its `history`; at startup, a
  directory without a live shim, as after a reboot or crash, is a frozen
  session with status "lost" and the last saved screen. A shim whose
  program exited while no tstermd ran is attached anyway, so its last
  output is read before it freezes. Frozen sessions don't hold their
  names. A frozen session's page has a close button in its header, and
  closing the current tab shows the next tab's session, right or else
  left. A viewer of a session that ends keeps its connection, for
  history.
- **Floods stay interruptible.** As with Mosh, a program spraying output
  (`yes`, `find /`) can't bury a Ctrl-C. The shim reads the PTY only 64 KB
  ahead of what an attached tstermd has taken (the 16 MB limit is for when
  none is attached), so a program faster than tstermd waits in its own
  writes, and Ctrl-C's SIGINT (with the kernel's flush of the PTY's output)
  takes effect at once. WebSocket Deltas come at most every 20 ms, and
  viewers with `?acks=1` (the web UI) acknowledge each once drawn
  (`requestAnimationFrame`), with at most 2 unacknowledged; background tabs,
  which don't draw, get one a second. The web viewer skips to the newest
  history when far behind and keeps 5000 lines while following. Measured
  (Ctrl-C to prompt): `yes` 2.9 s → 40 ms; `find /` with a viewer taking
  100 ms a frame 11.5 s → 0.4 s; in headless Chromium, 37-75 ms.
- **Performance.** Writes: 132 MB/s (charm: 15). Snapshot of 200×60:
  1.4 ms (charm: 0.7); fix by snapshotting only dirty rows (the render state
  tracks them). Each terminal: ~2 MB of wasm memory at start (1 MB is the
  stack, which could shrink), growing with scrollback (16 MB cap; libghostty
  can compress history); wasm memory never shrinks. Module compile: ~0.3 s
  at startup (wazero can cache to disk).
- **Next.** Dirty-row snapshots; set default
  colors from the client's theme (`SetColors` in `pkg/vt/ghostty.go`); pin a
  stable libghostty release artifact rather than building from the tag;
  checkpoints carry the whole scrollback every 256 KB or 5s of output, and
  the shim protocol sends them as base64 JSON, so a binary frame for the
  blob may be worth it.

## Desktop GUI (Fyne)

`cmd/tsterm-gui [HOST[:PORT]]` (`internal/gui`) is a Fyne desktop app for
Linux, macOS, and Windows, started 2026-11-12, aiming at parity with the web
UI. It talks to tstermd exactly as the web UI does, over HTTP and the
WebSocket (with acks), through `pkg/wsclient`, a UI-free client core (API,
stream, screen state, history loading, line wrapping, key/paste/mouse
encoding, palette), tested against real handlers. Not the UDP protocol yet.

- **Has:** tabs of the open set (close ×, Other (N), +), the session list
  (new, demo, rename, reopen, delete forever), the terminal view (the rows in
  view of history plus screen in a `TextGrid`, virtual scrolling with a
  thumb, older history loading near the top, wheel and Shift-PageUp/Down,
  pinned to the bottom, skip-ahead on floods; sideways, with a sideways
  wheel or Shift-wheel, across a session wider than the window), colors and bold/italic/
  underline/faint/reverse/conceal, wide characters, cursor shapes with blink
  and hollow when unfocused, the checkerboard, selection and copy
  (Ctrl-Shift-C on Linux/Windows, Cmd-C on macOS) and paste (Ctrl-Shift-V,
  Shift-Insert, Cmd-V, bracketed), xterm key encodings (Ctrl, Alt as ESC,
  application cursor mode), mouse reports (SGR/X10; Shift selects instead),
  hyperlinks (click), the bell (flash), notifications (in-window toast while
  focused, else the system's), focus reports, auto-fit (on by default, a
  per-install preference) and fit once with size leadership, view-only mode,
  ended sessions (status, close), reconnecting. It remembers the last host.
- **Packaging:** run as a bare binary, it gets a generic Dock icon on
  macOS, which shows an app bundle's icon (GLFW ignores window icons
  there). `make mac-install`, on a Mac, makes `tsterm.app`, with
  `Icon.png` (a placeholder icon), using Fyne's packaging tool, a tool
  dependency in go.mod (`go tool fyne`), and copies it to /Applications;
  `make mac-app` just makes it, as CI does. The app's details are flags
  in the Makefile rather than a `FyneApp.toml`, which the tool rewrites,
  bumping its build number, on every run. On Linux and Windows, the
  embedded icon is set at run time.
- **Building:** Fyne needs cgo. Linux builds natively (needs the GL/X11 dev
  headers); Windows cross-compiles with mingw
  (`CGO_ENABLED=1 GOOS=windows CC=x86_64-w64-mingw32-gcc`, add
  `-ldflags=-H=windowsgui` for no console window); macOS needs Apple's SDK,
  so only a Mac builds it. `CGO_ENABLED=0 GOOS=darwin go build -tags ci
  ./internal/gui ./pkg/wsclient` type-checks all of the GUI but its `main`
  (Fyne's `ci` tag swaps out its cgo parts). `.github/workflows/build.yml`
  builds it on Linux, macOS, and Windows runners, and runs the tests.
- **Tested** on Linux under Xvfb with xdotool and ImageMagick's `import`,
  against a separate tstermd (`tstermd run -port 2300 -udp-port 2301
  -run-dir /tmp/... -sessions-dir /tmp/... -shim-launch direct`), so as not
  to resize real sessions: keys, mouse reports, notifications, history,
  tabs, the list, ended sessions, copy and paste, auto-fit, the
  checkerboard, reconnecting, renaming, and deleting. Windows and macOS
  builds are untested at run time.
  Closing a running session's tab flies a copy of it into Other; a
  double-click renames the current tab; strikethrough is drawn over the
  grid.
- **Fonts:** Fyne draws text in its theme's font and falls back to its
  bundled ones (DejaVu Sans Mono; EmojiOne, in color; a small symbol font)
  and then to system fonts, but only those it indexes under the
  character's script, so symbols in the Common script, as Claude Code's
  ⏵ ⎿ ⧉ and braille spinners, came out as boxes even where the system has
  them. Cells whose characters DejaVu Sans Mono lacks, and that aren't
  emoji (by default, or with U+FE0F), are drawn in Fyne's Symbol text
  style, for which the theme returns an embedded font that covers them,
  Tsterm Symbols, JuliaMono's symbol ranges (OFL; 440 KB, subset and
  renamed as `internal/gui/fonts/README` says), in `internal/gui/fonts.go`.
- **Gaps vs. the web UI:** no audible bell (Fyne has no audio); no
  overline or curly/dotted/dashed/double underlines (TextGrid draws one
  kind); a cell is one rune, so emoji sequences (skin tones, ZWJ) show
  only their first; a colored background can cover a wide character's
  right half; Other's count doesn't pulse; no
  mobile extra-keys bar; IME composition untested; the auto-fit preference
  isn't shared with browsers.

## Upcoming work

- **Frozen sessions, more.** "Restart" a frozen session in the same
  directory (needs OSC 7, which libghostty tracks); configurable retention;
  a "keep forever" pin; renaming frozen sessions; paging and search in the
  session list's Ended section (it shows the newest 100).
- **Object storage.** Push old history segments and frozen sessions off
  to object storage (S3 and the like), keeping their index locally and
  fetching them when looked at; segments never change once written, which
  suits it. Then retention could be far longer.
- **Open set, more.** A shared history budget across sessions (now 256 MB
  each); per-device tab sets, if wanted; dragging tabs to reorder; what a
  closed session's bell and notifications should do (now nothing).
- **History, more.** Search in the web UI (`history.Store.Search` scans;
  an index later); virtualize the web viewer's history (it keeps every
  line it has loaded in the DOM); a scroll op in screen diffs (scrolling
  output resends every row, over UDP too); shrink libghostty's in-memory
  scrollback, and so checkpoints, now that history is on disk; history
  for GUI clients over UDP (a request/response like the WebSocket's).
- **GUI clients**: iOS, Android, macOS, Linux, later Windows. Plan: extract
  a Go client core from `cmd/tsterm` into `pkg/client` (UDP state sync,
  reassembly, reconnect, input sequencing, key encoding), built with gomobile
  for mobile and as a C archive for desktop, with thin native UIs (Swift for
  Apple, Kotlin for Android, GTK4 for Linux). Clients embed **tsnet** and
  **tailcat** (~/src/github.com/tailscale/tailcat: Tailscale's data plane
  without its control plane, peers connecting by out-of-band address), so
  they need no Tailscale app. Rendering is a grid of styled glyphs (font
  fallback, graphemes, procedural box drawing); no VT parsing on clients.
- **tstermd as a tailcat listener**, alongside the tailnet listeners.
- **Read-only share links**: WebSocket connections without `?write=1` are
  already view-only; needs a way to grant non-owners view access.
- **Protocol gaps** for GUI clients: scrollback; client theme colors (the
  emulator answers color queries with its own defaults).
- **Web UI**: keys browsers keep (Ctrl-W, Ctrl-T); a font-size control, for
  phones; trying it on real iOS and Android keyboards (only emulated so far).
- **Model gaps**: underline color (SGR 58) and overline aren't in
  `vt.Style`, so clients don't draw them; blink isn't animated.
- **Naming**: user aliases exist (rename); optional window-title prefixing
  with the alias is still to do.

## Maybe future

libghostty supports these, but each needs a security model first: what a
program in a session may do to the user's machine through a client, and
especially what a page in a browser may do. And each needs working out for
the web UI, where browsers restrict or lack the equivalent.

- **OSC 52 clipboard.** A program writing the user's clipboard, and
  especially reading it, is a data-exfiltration channel. Would need per-
  session consent; browsers only allow clipboard access on a user gesture.
- **Kitty keyboard protocol.** Less ambiguous key reporting; mostly a
  client-side encoding question, but browsers don't expose every key.
- **Shell-integration markers** (OSC 133 prompts, OSC 7 working directory).
  Useful for prompt navigation and "open a new session here", but they're
  program-controlled claims to treat as untrusted.
- **Kitty graphics** (images). Needs an image transport in the protocol and
  bounds on memory; image decoding of untrusted data.

## TODOs and known gaps

- Wake-from-sleep detection polls every 5s; use OS wake events instead
  (IOKit on macOS, logind's PrepareForSleep on Linux). TODOs in
  `cmd/tsterm/attach.go`.
- tstermd doesn't reopen its sockets if its own Tailscale IPs change.
- `tstermd install` is for a user's systemd instance only: not root, or a
  system-wide service, or macOS's launchd.
- Shims under launchd (macOS) aren't handled; peer-credential checks on
  macOS/BSDs (`getpeereid`) TODO in `pkg/shim/peer_other.go`.
- Input could be applied twice if the server applies a keystroke, its ack is
  lost, and the link stays one-way broken for the whole idle timeout.
- Size leadership ignores mousing over a window (only focus, typing,
  resizing a focused window, attaching, and auto-fit or fit once count),
  so that moving the mouse across windows doesn't flip sizes. Revisit if
  focus turns out not to be enough.
- Hyperlink URIs and graphemes over 256 bytes are dropped by the glue.
- No remote yet; history before 2026-11-10 was reconstructed approximately.

## Conventions

- Branch `main`. Commit messages follow Tailscale style (`pkg: summary`).
- "Session" is the user-facing noun for a PTY; "window" and "tab" are kept
  for whatever a GUI renders sessions in.
