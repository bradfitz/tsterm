// Code shared by the tsterm web pages.
"use strict";

// startRename replaces el's contents with a text field for renaming the
// session id, currently named current. Enter saves, through the API;
// Escape or leaving the field cancels. On success, onDone is called with
// the new name, and is responsible for redrawing el. An empty name
// removes the session's name.

function startRename(el, id, current, onDone) {
  const saved = Array.from(el.childNodes);
  const input = document.createElement("input");
  input.type = "text";
  input.value = current || "";
  input.placeholder = "name";
  input.size = Math.max(12, input.value.length + 2);
  input.spellcheck = false;
  el.replaceChildren(input);
  input.focus();
  input.select();

  let busy = false, done = false;
  const cancel = () => { if (!busy && !done) el.replaceChildren(...saved); };
  input.addEventListener("keydown", async (ev) => {
    ev.stopPropagation();
    if (ev.key === "Escape") { cancel(); return; }
    if (ev.key !== "Enter") return;
    busy = true;
    const name = input.value.trim();
    const resp = await fetch(`/v1/sessions/${encodeURIComponent(id)}`, {
      method: "PATCH",
      headers: {"Content-Type": "application/json", "Accept": "application/json"},
      body: JSON.stringify({name}),
    });
    busy = false;
    if (!resp.ok) {
      let msg = resp.statusText;
      try { msg = (await resp.json()).message || msg; } catch {}
      alert(`Can't rename: ${msg}`);
      input.focus();
      return;
    }
    done = true;
    onDone(name);
  });
  input.addEventListener("blur", cancel);
}

// demoName is the session name that starts tsterm's demo program, which
// shows off what a terminal can do.
const demoName = "_tsterm_demo";

// newSession starts a session, named name unless it is empty, and opens
// it with open, by default by going to its page. Pages fit new sessions to
// the window once: nothing else owns their size yet.
async function newSession(name, open = (id) => { location.href = `/s/${encodeURIComponent(id)}?fit`; }) {
  const resp = await fetch("/v1/sessions", {
    method: "POST",
    headers: {"Content-Type": "application/json", "Accept": "application/json"},
    body: JSON.stringify(name ? {name} : {}),
  });
  if (!resp.ok) {
    let msg = resp.statusText;
    try { msg = (await resp.json()).message || msg; } catch {}
    alert(`Can't create a session: ${msg}`);
    return;
  }
  const info = await resp.json();
  open(info.id, info.name || "");
}
