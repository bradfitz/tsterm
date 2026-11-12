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
  live in a `tsterm-shim` process (one per session; under systemd, its own
  transient unit `tsterm-pty-<ID>`), so tstermd can restart or upgrade
  without killing sessions. The tstermd/shim protocol is `pkg/shimproto`.
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
- **Web UI** (`http://HOST:2200/`, owner only): session list with renaming
  and creation; per-session viewer with typing (on by default), view-only
  mode, auto-fit (size-owning) mode, mouse input, and tabs of recently
  active sessions in the one header bar (scrolling sideways, by wheel too,
  when they don't fit; double-click the current one to rename it). The
  cursor follows the program's shape and blink, like Ghostty: an inverted
  block by default, hollow while the page isn't focused. Tabs switch
  sessions in place (a new WebSocket, `history.pushState`; back and forward
  work), without reloading the page. Auto-fit is one preference for the
  whole web app, shared by its windows through localStorage, and applied
  by writable viewers. It fills the whole terminal area; its toggle, and a
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
  "Naming"). Every session's TERM is xterm-256color (tstermd's emulator),
  and LANG defaults to C.UTF-8.

Run it: `go install ./cmd/...`, then
`systemd-run --user --unit=tstermd --collect ~/go/bin/tstermd`. User
services need lingering (`loginctl enable-linger`).

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
  encoding) is versioned by `TSTERM_ABI_VERSION`, checked at load. Licenses:
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
- **Performance.** Writes: 132 MB/s (charm: 15). Snapshot of 200×60:
  1.4 ms (charm: 0.7); fix by snapshotting only dirty rows (the render state
  tracks them). Each terminal: ~2 MB of wasm memory at start (1 MB is the
  stack, which could shrink), growing with scrollback (16 MB cap; libghostty
  can compress history); wasm memory never shrinks. Module compile: ~0.3 s
  at startup (wazero can cache to disk).
- **Next.** Dirty-row snapshots; expose scrollback to clients; set default
  colors from the client's theme (`SetColors` in `pkg/vt/ghostty.go`); pin a
  stable libghostty release artifact rather than building from the tag;
  checkpoints carry the whole scrollback every 256 KB or 5s of output, and
  the shim protocol sends them as base64 JSON, so a binary frame for the
  blob may be worth it.

**To do:** push tsterm-prebuilt's commits (through 0b42044), then run
`go get github.com/bradfitz/tsterm-prebuilt/ghosttyvt@main` and remove the
`replace` directive from go.mod.

## Upcoming work

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
- Shims under launchd (macOS) aren't handled; peer-credential checks on
  macOS/BSDs (`getpeereid`) TODO in `pkg/shim/peer_other.go`.
- Input could be applied twice if the server applies a keystroke, its ack is
  lost, and the link stays one-way broken for the whole idle timeout.
- Two viewers with auto-fit on fight over the session's size (more often
  now that auto-fit is app-wide, as two windows on one session both have
  it): each refits
  to its own window whenever its area changes, so the size flips to
  whichever resized last. Idea: one auto-fit leader per session, tracked by
  the server. Turning auto-fit on, or interacting with a viewer (typing,
  resizing its window, mousing over it), makes that viewer the leader. Only
  the leader's resizes apply; others show the leader's size (checkerboard
  or fit once) and say who has it. The CLI's resizes should take part too.
- Hyperlink URIs and graphemes over 256 bytes are dropped by the glue.
- No remote yet; history before 2026-11-10 was reconstructed approximately.

## Conventions

- Branch `main`. Commit messages follow Tailscale style (`pkg: summary`).
- "Session" is the user-facing noun for a PTY; "window" and "tab" are kept
  for whatever a GUI renders sessions in.
