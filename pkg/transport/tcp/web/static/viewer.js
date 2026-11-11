// tsterm session viewer. It connects to the session's WebSocket stream,
// which sends a CBOR Snapshot and then Deltas (see pkg/proto and pkg/vt for
// the field names), and renders the screen. Unless it is view-only, it also
// sends what the user types, and, in size-owning mode, resizes the session
// to fit the browser window.
"use strict";

// decodeCBOR decodes one CBOR item: the subset the server's encoder emits.
function decodeCBOR(buf) {
  const view = new DataView(buf);
  const text = new TextDecoder();
  let pos = 0;
  function length(ai) {
    if (ai < 24) return ai;
    switch (ai) {
      case 24: return view.getUint8(pos++);
      case 25: pos += 2; return view.getUint16(pos - 2);
      case 26: pos += 4; return view.getUint32(pos - 4);
      case 27: {
        // Sequence numbers (the bell's, notifications') exceed 2^53, so
        // keep large values exact as BigInts, which compare correctly
        // with Numbers.
        pos += 8;
        const v = view.getBigUint64(pos - 8);
        return v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : v;
      }
    }
    throw new Error("unsupported CBOR length " + ai);
  }
  function item() {
    const b = view.getUint8(pos++);
    const major = b >> 5, ai = b & 31;
    switch (major) {
      case 0: return length(ai);
      case 1: return -1 - length(ai);
      case 2: { const n = length(ai); pos += n; return new Uint8Array(buf, pos - n, n); }
      case 3: { const n = length(ai); pos += n; return text.decode(new Uint8Array(buf, pos - n, n)); }
      case 4: { const n = length(ai), a = new Array(n); for (let i = 0; i < n; i++) a[i] = item(); return a; }
      case 5: { const n = length(ai), m = {}; for (let i = 0; i < n; i++) { const k = item(); m[k] = item(); } return m; }
      case 6: length(ai); return item(); // tag: ignore it
      case 7:
        switch (ai) {
          case 20: return false;
          case 21: return true;
          case 22: case 23: return null;
          case 25: pos += 2; return view.getUint16(pos - 2); // half floats: not used
          case 26: pos += 4; return view.getFloat32(pos - 4);
          case 27: pos += 8; return view.getFloat64(pos - 8);
        }
    }
    throw new Error("unsupported CBOR initial byte " + b);
  }
  return item();
}

// The xterm 256-color palette.
const palette = (() => {
  const p = ["#000000", "#cd0000", "#00cd00", "#cdcd00", "#0000ee", "#cd00cd", "#00cdcd", "#e5e5e5",
             "#7f7f7f", "#ff0000", "#00ff00", "#ffff00", "#5c5cff", "#ff00ff", "#00ffff", "#ffffff"];
  const level = [0, 95, 135, 175, 215, 255];
  const hex = (r, g, b) => "#" + [r, g, b].map(v => v.toString(16).padStart(2, "0")).join("");
  for (let r = 0; r < 6; r++) for (let g = 0; g < 6; g++) for (let b = 0; b < 6; b++)
    p.push(hex(level[r], level[g], level[b]));
  for (let i = 0; i < 24; i++) { const v = 8 + 10 * i; p.push(hex(v, v, v)); }
  return p;
})();

// color converts a vt.Color ({k, i, r, g, b}) to CSS, or null for default.
function color(c) {
  if (!c) return null;
  switch (c.k) {
    case 1: return palette[c.i || 0];
    case 2: return `rgb(${c.r || 0},${c.g || 0},${c.b || 0})`;
  }
  return null;
}

// Attribute bits, as in vt.Attr.
const BOLD = 1, FAINT = 2, ITALIC = 4, REVERSE = 32, CONCEAL = 64, STRIKE = 128;

const screenEl = document.getElementById("screen");
const statusEl = document.getElementById("status");
const sessionID = screenEl.dataset.id;

// The screen being displayed: its size, rows of cells, the cursor, and the
// terminal modes (a vt.Modes: k is application cursor keys, p bracketed
// paste, f focus events).
let numRows = 0, numCols = 0, rows = [], cursor = null, modes = {};

// setSize sizes the screen element to exactly the terminal, so that the
// out-of-bounds pattern shows around it.
function setSize(r, c) {
  numRows = r; numCols = c;
  screenEl.style.width = `${c}ch`;
  screenEl.style.height = `${r * 1.2}em`; // the CSS line-height
}

function renderRow(cells, y) {
  const line = document.createElement("div");
  for (let x = 0; x < cells.length; x++) {
    const cell = cells[x] || {};
    if (!cell.w) continue; // continuation of a wide character
    const span = document.createElement("span");
    span.textContent = cell.g || " ";
    const st = cell.s || {}, attrs = st.a || 0;
    let fg = color(st.f), bg = color(st.b);
    if (attrs & REVERSE) { [fg, bg] = [bg || "#000", fg || "#ddd"]; }
    if (fg) span.style.color = fg;
    if (bg) span.style.backgroundColor = bg;
    const cls = [];
    if (attrs & BOLD) cls.push("bold");
    if (attrs & FAINT) cls.push("faint");
    if (attrs & ITALIC) cls.push("italic");
    if (st.u) cls.push("underline");
    if (attrs & STRIKE) cls.push("strike");
    if (attrs & CONCEAL) cls.push("conceal");
    if (cursor && cursor.v && cursor.r === y && cursor.c === x) cls.push("cursor");
    if (cls.length) span.className = cls.join(" ");
    if (cell.l) {
      const a = document.createElement("a");
      a.href = cell.l; a.target = "_blank"; a.rel = "noopener";
      a.appendChild(span);
      line.appendChild(a);
    } else {
      line.appendChild(span);
    }
  }
  return line;
}

function renderAll() {
  screenEl.replaceChildren(...rows.map(renderRow));
}

function rerenderRow(y) {
  if (y >= 0 && y < rows.length && screenEl.children[y])
    screenEl.children[y].replaceWith(renderRow(rows[y], y));
}

function applySnapshot(s) {
  setSize(s.r || 0, s.c || 0);
  // A notification or bell from before this snapshot is old news.
  lastNotification = (s.n && s.n.s) || 0;
  lastBell = s.b || 0;
  rows = s.x || [];
  cursor = s.cur || null;
  modes = s.m || {};
  if (s.t) document.title = s.t;
  renderAll();
}

function applyDelta(d) {
  if (d.rz) {
    // Resized: the delta carries every row.
    setSize(d.r, d.c);
    rows = Array.from({length: d.r}, (_, y) => rows[y] || []);
  }
  const touched = new Set();
  for (const run of d.x || []) {
    const row = rows[run.r] = (rows[run.r] || []).slice();
    (run.x || []).forEach((cell, i) => { row[run.c + i] = cell; });
    touched.add(run.r);
  }
  if (d.mc) modes = d.m || {};
  if (d.ns && d.n && (d.n.s || 0) > lastNotification) {
    lastNotification = d.n.s;
    // A full screen, as on connecting, only brings us up to date.
    if (!d.rz) showNotification(d.n);
  }
  if ((d.b || 0) > lastBell) {
    lastBell = d.b;
    if (!d.rz) ringBell();
  }
  const old = cursor;
  cursor = d.cur || cursor;
  if (old) touched.add(old.r);
  if (cursor) touched.add(cursor.r);
  if (d.ts) document.title = d.t || sessionID;
  if (d.rz) renderAll(); else touched.forEach(rerenderRow);
}

// Options, from the URL: ?ro for view-only, ?fit to start owning the size.
const params = new URLSearchParams(location.search);
let writable = !params.has("ro");
let sizeOwner = params.has("fit");

let ws = null;

function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const q = writable ? "?write=1" : "";
  const sock = ws = new WebSocket(`${proto}//${location.host}/v1/sessions/${encodeURIComponent(sessionID)}/ws${q}`);
  sock.binaryType = "arraybuffer";
  sock.onopen = () => {
    showStatus();
    if (sizeOwner) fit();
  };
  sock.onmessage = (ev) => {
    const env = decodeCBOR(ev.data);
    if (env[6]) applySnapshot(env[6]["s_"]);
    else if (env[5]) applyDelta(env[5].d);
    else if (env[7]) { statusEl.textContent = "session ended"; sock.onclose = null; ws = null; }
  };
  sock.onclose = () => {
    if (ws !== sock) return; // replaced by a reconnect
    ws = null;
    statusEl.textContent = "disconnected; retrying…";
    setTimeout(connect, 2000);
  };
}

function showStatus() {
  if (!ws) return;
  statusEl.textContent = !writable ? "view-only" : sizeOwner ? "live, sizing to window" : "live";
}

// reconnect reopens the stream, as after switching between view-only and
// writable, which the server decides per connection.
function reconnect() {
  const old = ws;
  ws = null;
  if (old) { old.onclose = null; old.close(); }
  connect();
}

function send(msg) {
  if (writable && ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
}

connect();

// Typing. keyBytes translates a key press into what a terminal sends for
// it, following xterm: modifiers on special keys as CSI 1;M sequences,
// Ctrl+letter as a control character, Alt as an ESC prefix. Keys with the
// Meta (Cmd) key are left to the browser, for copy and paste.
//
// ev may be a KeyboardEvent, or any object with its key and modifier
// fields, as from the extra-keys bar.
function keyBytes(ev) {
  if (ev.isComposing || ev.metaKey) return null;
  const k = ev.key;
  // Ctrl-Shift-C and Ctrl-Shift-V copy and paste, as in desktop terminals.
  if (ev.ctrlKey && ev.shiftKey && (k === "C" || k === "V")) return null;
  const mod = (ev.shiftKey ? 1 : 0) | (ev.altKey ? 2 : 0) | (ev.ctrlKey ? 4 : 0);
  // cursor returns a cursor-style key: SS3 in application cursor mode.
  const cursorKey = (final) =>
    mod ? `\x1b[1;${mod + 1}${final}` : (modes.k ? `\x1bO${final}` : `\x1b[${final}`);
  const tilde = (n) => mod ? `\x1b[${n};${mod + 1}~` : `\x1b[${n}~`;
  const fkey = (final) => mod ? `\x1b[1;${mod + 1}${final}` : `\x1bO${final}`;
  switch (k) {
    case "Enter": return ev.altKey ? "\x1b\r" : "\r";
    case "Backspace": return ev.ctrlKey ? "\x08" : ev.altKey ? "\x1b\x7f" : "\x7f";
    case "Tab": return ev.shiftKey ? "\x1b[Z" : "\t";
    case "Escape": return "\x1b";
    case "ArrowUp": return cursorKey("A");
    case "ArrowDown": return cursorKey("B");
    case "ArrowRight": return cursorKey("C");
    case "ArrowLeft": return cursorKey("D");
    case "Home": return cursorKey("H");
    case "End": return cursorKey("F");
    case "Insert": return tilde(2);
    case "Delete": return tilde(3);
    case "PageUp": return tilde(5);
    case "PageDown": return tilde(6);
    case "F1": return fkey("P");
    case "F2": return fkey("Q");
    case "F3": return fkey("R");
    case "F4": return fkey("S");
  }
  const fn = {F5: 15, F6: 17, F7: 18, F8: 19, F9: 20, F10: 21, F11: 23, F12: 24}[k];
  if (fn) return tilde(fn);
  if ([...k].length !== 1) return null; // Shift, Control, and other non-characters
  if (ev.ctrlKey && !ev.altKey) {
    const c = k.toUpperCase().charCodeAt(0);
    if (c >= 0x40 && c <= 0x5f) return String.fromCharCode(c - 0x40); // Ctrl-A is 1, and so on
    const special = {" ": "\x00", "2": "\x00", "3": "\x1b", "4": "\x1c", "5": "\x1d", "6": "\x1e", "7": "\x1f", "8": "\x7f", "/": "\x1f", "?": "\x7f"}[k];
    return special ?? null;
  }
  // Alt plus an ASCII key is ESC-prefixed. On a Mac, Option makes other
  // characters instead, such as "∂", which are typed as they are.
  if (ev.altKey && k.charCodeAt(0) < 0x80) return "\x1b" + k;
  return k;
}

// Typing. Text arrives through kbdEl, a hidden text field, because that's
// the only way to get a mobile keyboard, which shows while kbdEl has the
// focus, and because mobile keyboards and IMEs deliver text as input
// events, not key events (Android reports most keys as "Unidentified").
// Key events still handle special keys and modifier combinations.
//
// Typing goes to the session wherever the page's focus is, except in other
// text fields (such as for renaming).
const kbdEl = document.getElementById("kbd");
const inTextField = (el) => el instanceof Element && el !== kbdEl && el.closest("input, textarea, select");

// The field always holds one character, so that a mobile keyboard's
// Backspace has something to delete, and so produces an input event.
const kbdFill = " ";
function resetField() {
  kbdEl.value = kbdFill;
  kbdEl.setSelectionRange(kbdFill.length, kbdFill.length);
}
resetField();

// Sticky modifiers, from the extra-keys bar, apply to the next key.
let stickyCtrl = false, stickyAlt = false;

function clearSticky() {
  stickyCtrl = stickyAlt = false;
  for (const b of document.querySelectorAll("#extrakeys .sticky")) b.classList.remove("on");
}

// typeText sends text typed into the field, applying sticky modifiers to
// its first character.
function typeText(text) {
  if (!text) return;
  if (stickyCtrl || stickyAlt) {
    const [first, ...rest] = Array.from(text);
    const b = keyBytes({key: first, ctrlKey: stickyCtrl, altKey: stickyAlt}) ?? first;
    clearSticky();
    text = b + rest.join("");
  }
  send({input: text});
}

document.addEventListener("keydown", (ev) => {
  if (!writable || inTextField(ev.target)) return;
  // Mobile keyboards and IMEs: their text comes as input events.
  if (ev.isComposing || ev.key === "Unidentified" || ev.keyCode === 229) return;
  const plain = [...ev.key].length === 1 && !ev.ctrlKey && !ev.altKey && !ev.metaKey;
  if (plain && !stickyCtrl && !stickyAlt && document.activeElement === kbdEl) {
    return; // the field's input event will deliver it
  }
  const b = keyBytes(stickyCtrl || stickyAlt
    ? {key: ev.key, shiftKey: ev.shiftKey, ctrlKey: ev.ctrlKey || stickyCtrl, altKey: ev.altKey || stickyAlt}
    : ev);
  if (b == null) return;
  ev.preventDefault();
  clearSticky();
  send({input: b});
});

// Composition (IMEs, and mobile keyboards' autocorrect) edits text in
// place before committing it. Send it as it changes, as the difference from
// what was sent so far: Backspaces for what changed, then the new text.
let composed = [];
function sendComposition(text) {
  const next = Array.from(text || "");
  let common = 0;
  while (common < composed.length && common < next.length && composed[common] === next[common]) common++;
  const out = "\x7f".repeat(composed.length - common) + next.slice(common).join("");
  composed = next;
  if (out) send({input: out});
}
kbdEl.addEventListener("compositionstart", () => { composed = []; });
kbdEl.addEventListener("compositionend", (ev) => {
  if (writable) sendComposition(ev.data);
  composed = [];
  resetField();
});

kbdEl.addEventListener("input", (ev) => {
  if (!writable) {
    resetField();
    return;
  }
  if (ev.isComposing || ev.inputType === "insertCompositionText") {
    sendComposition(ev.data);
    return;
  }
  switch (ev.inputType) {
    case "insertText":
    case "insertReplacementText":
      typeText(ev.data);
      break;
    case "insertLineBreak":
    case "insertParagraph":
      send({input: "\r"});
      break;
    case "deleteContentBackward":
      send({input: "\x7f"});
      break;
    case "deleteWordBackward":
      send({input: "\x17"}); // Ctrl-W
      break;
  }
  resetField();
});

// focusKeyboard focuses the field, which on mobile shows the keyboard.
function focusKeyboard() {
  kbdEl.focus({preventScroll: true});
}
const touch = matchMedia("(pointer: coarse)").matches;
// On a desktop, the field takes the focus right away, and back from clicks.
// On mobile, focusing it pops up the keyboard, so that's left to the user.
if (writable && !touch) focusKeyboard();
screenEl.addEventListener("click", () => {
  if (writable && !(modes.m && !touch)) focusKeyboard();
});

document.addEventListener("paste", (ev) => {
  if (!writable || inTextField(ev.target)) return;
  let text = ev.clipboardData.getData("text/plain");
  if (!text) return;
  ev.preventDefault();
  text = text.replace(/\r?\n/g, "\r"); // Enter, as typed
  if (modes.p) text = "\x1b[200~" + text + "\x1b[201~"; // bracketed paste
  send({input: text});
});

// Mouse reports, for programs that asked for them (modes.m is a
// vt.MouseMode: 1 X10, 2 normal, 3 button-event, 4 any-event tracking;
// modes.ms is SGR encoding). Shift bypasses reporting, so the user can still
// select text, as in most terminals.
let mouseButton = -1; // the button held down, if any

function mouseCell(ev) {
  const [cw, ch] = cellSize();
  const r = screenEl.getBoundingClientRect();
  const x = Math.floor((ev.clientX - r.left) / cw);
  const y = Math.floor((ev.clientY - r.top) / ch);
  if (x < 0 || y < 0 || x >= numCols || y >= numRows) return null;
  return [x, y];
}

// mouseReport encodes a mouse event for the program: button is 0-2 for
// left, middle, and right, or 64/65 for the wheel; flags adds 32 for
// motion; release is a button release.
function mouseReport(button, x, y, release, ev) {
  let b = button;
  if (ev.shiftKey) b |= 4;
  if (ev.altKey) b |= 8;
  if (ev.ctrlKey) b |= 16;
  if (modes.ms) return `\x1b[<${b};${x + 1};${y + 1}${release ? "m" : "M"}`;
  // The X10 encoding can't say which button was released, and can't
  // address past column or row 223.
  if (release) b = 3 | (b & ~3);
  return "\x1b[M" + String.fromCharCode(32 + b, 32 + Math.min(x + 1, 223), 32 + Math.min(y + 1, 223));
}

function mouseWanted(ev) {
  return writable && modes.m && !ev.shiftKey;
}

screenEl.addEventListener("mousedown", (ev) => {
  if (!mouseWanted(ev)) return;
  const cell = mouseCell(ev);
  if (!cell || ev.button > 2) return;
  ev.preventDefault();
  mouseButton = ev.button;
  send({input: mouseReport(ev.button, cell[0], cell[1], false, ev)});
});
window.addEventListener("mouseup", (ev) => {
  if (mouseButton < 0) return;
  const button = mouseButton;
  mouseButton = -1;
  if (!mouseWanted(ev) || modes.m < 2) return; // X10 mode reports presses only
  const cell = mouseCell(ev) || [0, 0];
  send({input: mouseReport(button, cell[0], cell[1], true, ev)});
});
let lastMotion = "";
screenEl.addEventListener("mousemove", (ev) => {
  if (!mouseWanted(ev)) return;
  // Button-event tracking reports drags; any-event tracking, all motion.
  if (!(modes.m === 4 || (modes.m === 3 && mouseButton >= 0))) return;
  const cell = mouseCell(ev);
  if (!cell) return;
  const key = cell.join(",");
  if (key === lastMotion) return; // only when entering a new cell
  lastMotion = key;
  const button = mouseButton >= 0 ? mouseButton : 3;
  send({input: mouseReport(32 + button, cell[0], cell[1], false, ev)});
});
screenEl.addEventListener("wheel", (ev) => {
  if (!mouseWanted(ev) || ev.deltaY === 0) return;
  const cell = mouseCell(ev);
  if (!cell) return;
  ev.preventDefault();
  send({input: mouseReport(ev.deltaY < 0 ? 64 : 65, cell[0], cell[1], false, ev)});
}, {passive: false});
screenEl.addEventListener("contextmenu", (ev) => {
  if (mouseWanted(ev)) ev.preventDefault(); // right clicks go to the program
});

// Focus reports, for programs that asked for them.
window.addEventListener("focus", () => { if (modes.f) send({input: "\x1b[I"}); });
window.addEventListener("blur", () => { if (modes.f) send({input: "\x1b[O"}); });

// Buttons must not take the focus: a focused button would turn a typed
// Enter or space into a click.
for (const b of document.querySelectorAll("header button, #sizehandle, #snap")) {
  b.addEventListener("mousedown", (ev) => ev.preventDefault());
}

// Size-owning mode: the session is resized to fill the browser window,
// whenever the window changes size. The handle at the terminal's corner
// turns it on and off.
const handleEl = document.getElementById("sizehandle");

// cellSize measures a character cell of the terminal's font. The result is
// cached until the window resizes, since mouse events need it constantly.
let cachedCellSize = null;
window.addEventListener("resize", () => { cachedCellSize = null; });
function cellSize() {
  if (!cachedCellSize) cachedCellSize = measureCell();
  return cachedCellSize;
}

function measureCell() {
  const probe = document.createElement("span");
  probe.textContent = "M".repeat(100);
  probe.style.visibility = "hidden";
  screenEl.parentElement.appendChild(probe);
  const w = probe.getBoundingClientRect().width / 100;
  probe.remove();
  const h = parseFloat(getComputedStyle(screenEl).lineHeight);
  return [w, h];
}

function fit() {
  const [cw, ch] = cellSize();
  const area = document.querySelector("main");
  // Leave a margin for the handle, so it stays in view.
  const cols = Math.max(10, Math.floor(area.clientWidth / cw) - 3);
  const rows = Math.max(3, Math.floor(area.clientHeight / ch) - 2);
  if (rows !== numRows || cols !== numCols) send({resize: {rows, cols}});
}

function setSizeOwner(on) {
  sizeOwner = on;
  handleEl.classList.toggle("on", on);
  handleEl.title = on
    ? "Sizing the session to this window. Click to stop."
    : "Click to size the session to this window.";
  showStatus();
  if (on) fit();
}
handleEl.addEventListener("click", () => setSizeOwner(!sizeOwner));
setSizeOwner(sizeOwner);

// The snap button: when the terminal is bigger than the window, so that
// its corner handle is out of view, pointing near the window's bottom-right
// corner offers a one-time resize to fit, without resizing the window.
const snapEl = document.getElementById("snap");
const areaEl = document.querySelector("main");
const snapZone = 80; // pixels from the corner

function overflowing() {
  return areaEl.scrollWidth > areaEl.clientWidth || areaEl.scrollHeight > areaEl.clientHeight;
}

function showSnap(show) {
  if (show) {
    // Sit inside any scrollbars.
    snapEl.style.right = `${areaEl.offsetWidth - areaEl.clientWidth}px`;
    snapEl.style.bottom = `${areaEl.offsetHeight - areaEl.clientHeight}px`;
  }
  snapEl.classList.toggle("visible", show);
}

document.addEventListener("mousemove", (ev) => {
  const r = areaEl.getBoundingClientRect();
  const near = ev.clientX > r.right - snapZone && ev.clientY > r.bottom - snapZone;
  showSnap(writable && near && overflowing());
});
document.addEventListener("mouseleave", () => showSnap(false));
snapEl.addEventListener("click", () => {
  fit();
  showSnap(false);
});

let fitTimer;
window.addEventListener("resize", () => {
  if (!sizeOwner) return;
  clearTimeout(fitTimer);
  fitTimer = setTimeout(fit, 100);
});

// The view-only switch.
const modeEl = document.getElementById("mode");
function showMode() {
  modeEl.textContent = writable ? "⌨ typing" : "👁 view-only";
  modeEl.title = writable ? "Click to stop typing into the session" : "Click to type into the session";
  handleEl.hidden = !writable;
}
modeEl.addEventListener("click", () => {
  writable = !writable;
  if (!writable && sizeOwner) setSizeOwner(false);
  showMode();
  reconnect();
  if (writable && !touch) focusKeyboard();
});

// The extra-keys bar, for mobile keyboards' missing keys, and the button
// that shows and hides the keyboard.
function showKeyboardButtons() {
  const up = document.activeElement === kbdEl;
  for (const b of document.querySelectorAll(".kbdtoggle")) {
    b.classList.toggle("on", up);
    b.title = up ? "Hide the keyboard" : "Show the keyboard";
  }
}
kbdEl.addEventListener("focus", showKeyboardButtons);
kbdEl.addEventListener("blur", showKeyboardButtons);
for (const b of document.querySelectorAll(".kbdtoggle")) {
  b.addEventListener("click", () => {
    if (document.activeElement === kbdEl) kbdEl.blur();
    else focusKeyboard();
  });
}
const extraKeys = {
  esc: {key: "Escape"}, tab: {key: "Tab"},
  up: {key: "ArrowUp"}, down: {key: "ArrowDown"}, left: {key: "ArrowLeft"}, right: {key: "ArrowRight"},
  pipe: {key: "|"}, tilde: {key: "~"}, slash: {key: "/"}, dash: {key: "-"},
};
for (const b of document.querySelectorAll("#extrakeys button")) {
  // Keep the focus, and so the keyboard, where it is.
  b.addEventListener("pointerdown", (ev) => ev.preventDefault());
  b.addEventListener("click", () => {
    const k = b.dataset.key;
    if (k === "ctrl") { stickyCtrl = !stickyCtrl; b.classList.toggle("on", stickyCtrl); return; }
    if (k === "alt") { stickyAlt = !stickyAlt; b.classList.toggle("on", stickyAlt); return; }
    const spec = extraKeys[k];
    if (!spec) return;
    const bytes = keyBytes({...spec, ctrlKey: stickyCtrl, altKey: stickyAlt});
    clearSticky();
    if (bytes != null) send({input: bytes});
  });
}

// On mobile, the keyboard covers part of the window without resizing it;
// the visual viewport is what's left. Fit the page to it, so the extra keys
// stay above the keyboard and size-owning mode fits the space left.
if (window.visualViewport) {
  const fitViewport = () => {
    document.body.style.height = `${visualViewport.height}px`;
    cachedCellSize = null;
    if (sizeOwner) fit();
  };
  visualViewport.addEventListener("resize", fitViewport);
  fitViewport();
}
showMode();

// The bell: a short beep and a flash of the screen. Browsers let a page
// play sound only after the user interacts with it, so the audio context is
// created on the first key press or click.
let lastBell = 0;
let audio = null;
function unlockAudio() {
  if (!audio && window.AudioContext) audio = new AudioContext();
  if (audio && audio.state === "suspended") audio.resume();
}
document.addEventListener("keydown", unlockAudio, {capture: true});
document.addEventListener("mousedown", unlockAudio, {capture: true});

function ringBell() {
  screenEl.classList.remove("bell");
  void screenEl.offsetWidth; // restart the animation
  screenEl.classList.add("bell");
  if (!audio || audio.state !== "running") return;
  const osc = audio.createOscillator();
  const gain = audio.createGain();
  osc.frequency.value = 880;
  gain.gain.setValueAtTime(0.15, audio.currentTime);
  gain.gain.exponentialRampToValueAtTime(0.001, audio.currentTime + 0.15);
  osc.connect(gain).connect(audio.destination);
  osc.start();
  osc.stop(audio.currentTime + 0.15);
}

// Notifications, which programs ask for with OSC 9 or OSC 777. While the
// page is in front, they're shown in it, as toasts; otherwise, as desktop
// notifications, once the user allows those.
let lastNotification = 0;
const notifyEl = document.getElementById("notify");

const toastsEl = document.getElementById("toasts");

function showNotification(n) {
  const label = nameEl.textContent || sessionID;
  if (document.hasFocus() && !document.hidden) {
    showToast(n.t || label, n.b || "");
    return;
  }
  if (!("Notification" in window) || Notification.permission !== "granted") return;
  const note = new Notification(n.t || label, {body: n.b || "", tag: `tsterm-${sessionID}`});
  note.onclick = () => { window.focus(); note.close(); };
}

function showToast(title, body) {
  const el = document.createElement("div");
  el.className = "toast";
  const t = document.createElement("b");
  t.textContent = title;
  const b = document.createElement("div");
  b.textContent = body;
  el.append(t, b);
  const dismiss = () => el.remove();
  el.addEventListener("click", dismiss);
  toastsEl.append(el);
  setTimeout(dismiss, 8000);
}

function showNotifyButton() {
  if (!window.isSecureContext) {
    // Browsers offer notifications only to HTTPS pages.
    notifyEl.hidden = false;
    notifyEl.disabled = true;
    notifyEl.textContent = "🔕 notifications need HTTPS";
    notifyEl.title = "Browsers allow notifications only on HTTPS pages. Enable HTTPS certificates for your tailnet (DNS settings in the Tailscale admin console), and tstermd will serve HTTPS.";
    return;
  }
  if (!("Notification" in window)) return;
  const p = Notification.permission;
  notifyEl.hidden = p === "granted";
  notifyEl.textContent = p === "denied" ? "🔕 notifications blocked" : "🔔 notify me";
  notifyEl.title = p === "denied"
    ? "Notifications are blocked for this site in your browser's settings"
    : "Show desktop notifications from this session while the page is in the background";
}
notifyEl.addEventListener("click", async () => {
  await Notification.requestPermission();
  showNotifyButton();
});
showNotifyButton();

// The tab bar: the most recently active sessions, refreshed periodically,
// and a button to start a new one.
const tabsEl = document.getElementById("tabs");
const maxTabs = 12;

// tabLabel names a session for its tab: its name, or the start of its ID.
function tabLabel(s) {
  return s.name || s.id.slice(0, "pty-".length + 4);
}

async function refreshTabs() {
  let list;
  try {
    const resp = await fetch("/v1/sessions?sort=activity", {headers: {"Accept": "application/json"}});
    if (!resp.ok) return;
    list = await resp.json();
  } catch {
    return;
  }
  const tabs = list.filter(s => s.active).slice(0, maxTabs).map(s => {
    const el = document.createElement(s.id === sessionID ? "span" : "a");
    el.textContent = tabLabel(s);
    el.title = s.id + (s.name ? ` (${s.name})` : "");
    if (s.id === sessionID) el.className = "current";
    else el.href = `/s/${encodeURIComponent(s.id)}`;
    return el;
  });
  const add = document.createElement("button");
  add.textContent = "+";
  add.title = "New session (shift-click for the demo)";
  add.addEventListener("click", (ev) => newSession(ev.shiftKey ? demoName : ""));
  tabsEl.replaceChildren(...tabs, add);
}
refreshTabs();
setInterval(refreshTabs, 5000);

// Double-click the session's name or ID to rename it.
const identEl = document.getElementById("ident");
const nameEl = document.getElementById("name");
identEl.addEventListener("dblclick", () => {
  if (identEl.querySelector("input")) return;
  const saved = Array.from(identEl.childNodes);
  startRename(identEl, sessionID, nameEl.textContent, (name) => {
    nameEl.textContent = name;
    identEl.replaceChildren(...saved);
    document.title = `${name || sessionID} · tsterm`;
    refreshTabs();
  });
});
