// tsterm read-only session viewer. It connects to the session's WebSocket
// stream, which sends a CBOR Snapshot and then Deltas (see pkg/proto and
// pkg/vt for the field names), and renders the screen.
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
      case 27: pos += 8; return Number(view.getBigUint64(pos - 8));
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

// The screen being displayed: rows of cells, plus the cursor.
let rows = [], cursor = null;

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
  rows = s.x || [];
  cursor = s.cur || null;
  if (s.t) document.title = s.t;
  renderAll();
}

function applyDelta(d) {
  if (d.rz) {
    // Resized: the delta carries every row.
    rows = Array.from({length: d.r}, (_, y) => rows[y] || []);
  }
  const touched = new Set();
  for (const run of d.x || []) {
    const row = rows[run.r] = (rows[run.r] || []).slice();
    (run.x || []).forEach((cell, i) => { row[run.c + i] = cell; });
    touched.add(run.r);
  }
  const old = cursor;
  cursor = d.cur || cursor;
  if (old) touched.add(old.r);
  if (cursor) touched.add(cursor.r);
  if (d.ts) document.title = d.t || sessionID;
  if (d.rz) renderAll(); else touched.forEach(rerenderRow);
}

function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${location.host}/v1/sessions/${encodeURIComponent(sessionID)}/ws`);
  ws.binaryType = "arraybuffer";
  ws.onopen = () => { statusEl.textContent = "live (read-only)"; };
  ws.onmessage = (ev) => {
    const env = decodeCBOR(ev.data);
    if (env[6]) applySnapshot(env[6]["s_"]);
    else if (env[5]) applyDelta(env[5].d);
    else if (env[7]) { statusEl.textContent = "session ended"; ws.onclose = null; }
  };
  ws.onclose = () => {
    statusEl.textContent = "disconnected; retrying…";
    setTimeout(connect, 2000);
  };
}
connect();
