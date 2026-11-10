"use strict";

/* The size of the terminal: whose it is, how it is drawn, and what the strip
 * says of it.
 *
 * A terminal that is not held is fitted to the holder's window, as it always
 * was. A held session is never resized by this page on its own: it is drawn
 * at the session's size, scaled to this window's width by its font, from
 * 13 px down to 7 px, and whatever is still wider or taller pans inside the
 * screen. Its size changes only when somebody allowed to presses Fit.
 */

const TERM_FONT = 13, TERM_FONT_MIN = 7;
let termFont = TERM_FONT;
let termFontView = null;     // the view termFont is the font of
// termZoom is how a held session larger than the window is drawn: "fit",
// scaled to its width, or "actual", at 13 px and panned. A per-device
// convenience, read once.
let termZoom = (() => { try { return localStorage.getItem("rota.zoom") === "actual" ? "actual" : "fit"; } catch { return "fit"; } })();

let termWantSize = null, termSizeTimer = false;
// termFit: the holder's terminal is the size of the space it is in and says
// so; everybody else's is the size of the session. Only one of the two is
// the truth, and it belongs to whoever is typing. A held session is the
// size it is for everyone, and is only drawn to suit this window.
function termFit() {
  if (!termView) return;
  if (termHeld) { termView.resize(termCols, termRows); termScale(); return; }
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

// termSized follows a size the session now is, from a resized or a repaint
// frame: a held session is drawn at it and scaled again, whoever made it;
// a terminal this page is not typing into, or one shared from a window
// somewhere, is drawn at that size; the holder's own stays fitted to the
// space it is in.
function termSized() {
  if (!termView) return;
  if (termHeld) { termView.resize(termCols, termRows); termScale(); return; }
  if (termShared() || !termIHold()) termView.resize(termCols, termRows);
}

function termSendSize(size) {
  termWantSize = size;
  if (termSizeTimer) return;
  termSizeTimer = true;
  termLater(() => {
    termSizeTimer = false;
    const s = termWantSize; termWantSize = null;
    if (!s || !termSock || !termIHold() || termShared() || termHeld) return;
    try { termSock.send(JSON.stringify({ type: "resize", cols: s.cols, rows: s.rows })); } catch {}
  }, 150);
}

/* ============================================================ the font == */
// termScaleFont is the font at which cols columns take the room that cap
// columns take at cur px: never above 13 px, never below 7.
function termScaleFont(cap, cur, cols) {
  const f = Math.floor(cur * cap / cols);
  if (!Number.isFinite(f)) return Math.max(TERM_FONT_MIN, Math.min(TERM_FONT, cur));
  return Math.max(TERM_FONT_MIN, Math.min(TERM_FONT, f));
}

// A new emulator starts at 13 px, whatever the last one was drawn at.
function termFontFollow() {
  if (termFontView !== termView) { termFontView = termView; termFont = TERM_FONT; }
}

function termSetFont(px) {
  termFontFollow();
  if (px === termFont) return;
  termFont = px;
  try { if (typeof termView.font === "function") termView.font(px); } catch {}
}

// termPropose is how many cells fit the screen at the current font, or null
// when that cannot be said: an emulator that cannot measure, or a screen
// that is hidden.
function termPropose() {
  if (!termView || typeof termView.propose !== "function") return null;
  let p = null;
  try { p = termView.propose(); } catch { return null; }
  return p && p.cols > 0 && p.rows > 0 ? p : null;
}

// termScale draws a held session at the font that fits its columns to this
// window. Measured at a smaller font, the room is rounded down to whole
// cells, so the answer is taken again at the font it gave until it holds;
// three rounds are enough for any rounding.
function termScale() {
  if (!termView) return;
  termFontFollow();
  if (termZoom === "actual") { termSetFont(TERM_FONT); return; }
  for (let i = 0; i < 3; i++) {
    const p = termPropose();
    if (!p) return;
    const want = termScaleFont(p.cols, termFont, termCols);
    if (want === termFont) return;
    termSetFont(want);
  }
}

// termBig: at 13 px, this window has less room than the session has
// columns or rows. A font below 13 says so by itself.
function termBig() {
  if (!termView) return false;
  termFontFollow();
  if (termFont < TERM_FONT) return true;
  const p = termPropose();
  return !!p && (p.cols < termCols || p.rows < termRows);
}

/* ================================================================= Fit == */
// termFitSession makes a held session the size of this window, at 13 px,
// for everyone watching it. Nothing is sent unless this page may type, the
// keyboard is this person's or nobody's, the window has room, and, when
// others are watching, the person has said yes.
function termFitSession() {
  if (!termView || !termHeld || !termCanType()) return;
  if (termHolder && !termIHold()) return;
  termSetFont(TERM_FONT);
  let size = null;
  try { size = termView.fit(); } catch {}
  const c = size && size.cols, r = size && size.rows;
  // The view is the session's size again whatever is sent: it changes only
  // when the session says it has.
  const back = () => { termView.resize(termCols, termRows); termScale(); renderTermStrip(); };
  if (!(c > 0) || !(r > 0)) { back(); termSay("this window has no room to fit the session to"); return; }
  const others = termViewers.length - 1;
  if (others > 0 && !confirm(`Make ${termName(termInfo || { id: termId })} ${c}×${r} for the ${others} others watching it?`)) {
    back();
    return;
  }
  if (!termHoldsHere()) termTalk({ type: "claim" });
  termTalk({ type: "resize", cols: c, rows: r, fit: true });
  back();
}

function termZoomToggle() {
  termZoom = termZoom === "actual" ? "fit" : "actual";
  try { localStorage.setItem("rota.zoom", termZoom); } catch {}
  termScale();
  renderTermStrip();
}

/* =========================================================== the strip == */
// termSizeParts is what the strip shows of the size: the session's columns
// and rows; whether this window is too small for it; for a held session,
// how it is drawn then; and Fit, for whoever may type.
function termSizeParts() {
  const parts = [el("span", { class: "help", style: "margin:0", text: termCols + "×" + termRows })];
  const big = termBig();
  if (big) {
    const scaled = termHeld && termZoom !== "actual" && termFont < TERM_FONT;
    parts.push(el("span", { class: "help termbig", id: "term_big",
      text: scaled ? "larger than this window: drawn smaller" : "larger than this window" }));
  }
  if (termHeld && big) {
    const actual = termZoom === "actual";
    parts.push(el("button", { class: "btn tiny", id: "term_zoom", text: actual ? "Scaled" : "Actual size",
      title: actual ? "draw this session smaller, to the width of this window" : "draw this session at its own size, and pan",
      onclick: termZoomToggle }));
  }
  if (termHeld && termCanType()) {
    const theirs = !!termHolder && !termIHold();
    parts.push(el("button", { class: "btn tiny", id: "term_fit", text: "Fit", disabled: theirs,
      title: theirs ? `${termHolder} holds the keyboard; only its holder fits the session`
        : "make this session the size of this window, for everyone watching",
      onclick: termFitSession }));
  }
  return parts;
}
