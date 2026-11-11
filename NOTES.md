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
  mode, size-owning mode, snap-to-fit, and tabs of recently active sessions.
- **Sessions**: IDs are `pty-` + `crypto/rand.Text()`; optional names (see
  "Naming"). Every session's TERM is xterm-256color (tstermd's emulator),
  and LANG defaults to C.UTF-8.

Run it: `go install ./cmd/...`, then
`systemd-run --user --unit=tstermd --collect ~/go/bin/tstermd`. User
services need lingering (`loginctl enable-linger`).

## In progress: libghostty-vt as the emulator

Spike in `pkg/vt/ghosttyvt`: a `vt.Terminal` backed by libghostty-vt
(Ghostty's emulator, 1.0) compiled to wasm32 and run with wazero, so no cgo.
Not yet wired into tstermd.

- **Build.** The wasm module lives in its own repo and Go module,
  `github.com/bradfitz/tsterm-prebuilt/ghosttyvt` (local checkout:
  ~/src/github.com/bradfitz/tsterm-prebuilt), which exports it as
  `ghosttyvt.Wasm` so tsterm's history doesn't grow with each rebuild. Its
  `build.sh` fetches a pinned Zig (0.17.1) and the `vt-1.0.0` source, builds
  `libghostty-vt.a` for wasm32, links our glue (`glue/tsterm.c`) into
  `ghosttyvt.wasm` (~890 KB; reproducible), and generates `version.go`. The
  glue's contract with `pkg/vt/ghosttyvt` (exports, imports, snapshot
  encoding) is versioned by `TSTERM_ABI_VERSION`, checked at load. Licenses:
  Ghostty's MIT `LICENSE`, and `THIRD_PARTY_LICENSES` for the other linked
  code (uucode, translate_c, Zig's compiler_rt). The stock wasm build can't
  be used: it has no
  imports and delivers effects (such as replies to terminal queries) via C
  function pointers, which a wasm host can't supply. The glue forwards them to
  a host import, and encodes a whole screen in one call so the host crosses
  the wasm boundary once per snapshot instead of several times per cell.
- **Results.**
  - Screens identical to the current emulator on the test corpus (text,
    styles, wide characters, links, titles, modes, cursor, alt screen).
  - Replies to queries work (device attributes, cursor position, colors).
  - Handles 0x9C in titles (Claude Code's "✳") and OSC 8 links natively.
  - Full-fidelity checkpoints (`ghostty_snapshot_*`): both screens, scroll
    region, saved cursor, pen, pending wrap, unfinished escape sequences, and
    scrollback (500 lines: 6.5 KB). All cases our ANSI repaint can't do.
  - Write throughput 132 MB/s vs. 15 MB/s for charmbracelet/x/vt.
  - One wasm instance per terminal: a fault is contained to its session.
- **Costs.**
  - Snapshot of 200×60: 1.4 ms vs. 0.7 ms (0.9 ms in wasm, 0.55 ms decoding
    in Go). Fix: snapshot only dirty rows (the render state tracks them).
  - ~2 MB of wasm memory per terminal at start (1 MB is the stack, which
    could shrink), growing with scrollback (16 MB cap in the glue; libghostty
    can compress history). Wasm memory never shrinks.
  - Module compile: ~0.3 s once at startup (wazero can cache to disk).
  - The snapshot byte format is versioned separately from libghostty and not
    covered by its ABI promise, so a newer tstermd may not read an older
    checkpoint. Options: keep our ANSI repaint as a fallback in the
    checkpoint, or record the format version and refuse gracefully.
- **Next, if adopted.** Wire it in behind `pkg/vt` and drop charm and the
  workarounds below; switch shim checkpoints to libghostty snapshots; dirty-
  row snapshots; expose scrollback; set default colors from the client's
  theme (`Terminal.SetColors`); pin a stable libghostty release artifact
  rather than building from the tag; drop the `BoundaryScanner` (libghostty
  snapshots unfinished sequences).

Workarounds for charmbracelet/x/vt bugs (in `pkg/vt/charmvt.go`), moot if
libghostty replaces it: replies to queries written to an unread pipe
(blocked Write forever); 0x9C ending OSC strings; OSC 8 URI and params
swapped; cursor-blink callback inverted; unsynchronized `closed` flag. Its
public API also can't expose state needed for full checkpoints.

To update it: change and rebuild in tsterm-prebuilt, push, then run
`go get github.com/bradfitz/tsterm-prebuilt/ghosttyvt@main` here.

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
  emulator answers color queries with its own defaults); OSC 52 clipboard;
  OSC 9/777 notifications (useful for Claude Code); images (Kitty graphics).
- **Web UI**: mouse input; IME; keys browsers keep (Ctrl-W, Ctrl-T).
- **Naming**: user aliases exist (rename); optional window-title prefixing
  with the alias is still to do.

## TODOs and known gaps

- Wake-from-sleep detection polls every 5s; use OS wake events instead
  (IOKit on macOS, logind's PrepareForSleep on Linux). TODOs in
  `cmd/tsterm/attach.go`.
- tstermd doesn't reopen its sockets if its own Tailscale IPs change.
- Shims under launchd (macOS) aren't handled; peer-credential checks on
  macOS/BSDs (`getpeereid`) TODO in `pkg/shim/peer_other.go`.
- Input could be applied twice if the server applies a keystroke, its ack is
  lost, and the link stays one-way broken for the whole idle timeout.
- Hyperlink URIs and graphemes over 256 bytes are dropped by the ghosttyvt
  glue.
- No remote yet; history before 2026-11-10 was reconstructed approximately.

## Conventions

- Branch `main`. Commit messages follow Tailscale style (`pkg: summary`).
- "Session" is the user-facing noun for a PTY; "window" and "tab" are kept
  for whatever a GUI renders sessions in.
