"use strict";

/* The size of the terminal: whose it is, and what the strip says of it. */

let termWantSize = null, termSizeTimer = false;
// termFit: the holder's terminal is the size of the space it is in and says
// so; everybody else's is the size of the session. Only one of the two is
// the truth, and it belongs to whoever is typing.
function termFit() {
  if (!termView) return;
  // A shared terminal is never fitted, even by whoever holds its keyboard:
  // its size is the size of a window somebody is sitting at, and this page
  // cannot reach across and drag that. It is drawn at that size and scrolls
  // inside its container, which is what everybody but a holder always got.
  if (termShared() || !termIHold()) { termView.resize(termCols, termRows); return; }
  const size = termView.fit();
  if (!size || !size.cols || !size.rows) return;
  if (size.cols === termCols && size.rows === termRows) return;
  termSendSize(size);
}

function termSendSize(size) {
  termWantSize = size;
  if (termSizeTimer) return;
  termSizeTimer = true;
  termLater(() => {
    termSizeTimer = false;
    const s = termWantSize; termWantSize = null;
    if (!s || !termSock || !termIHold() || termShared()) return;
    try { termSock.send(JSON.stringify({ type: "resize", cols: s.cols, rows: s.rows })); } catch {}
  }, 150);
}

// termSizeParts is what the strip shows of the size: the session's columns
// and rows.
function termSizeParts() {
  return [el("span", { class: "help", style: "margin:0", text: termCols + "×" + termRows })];
}
