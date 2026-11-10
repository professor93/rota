"use strict";

/* The terminal page's emulator: the one file that names xterm.js. What it
 * hands the rest of the page is a small object, so a test can run the page
 * with a terminal that only records what it was shown, and so the emulator
 * could be replaced one day without reading the rest of the page.
 */

let termAssets = null;
// loadXterm fetches the emulator from this server — it is vendored into the
// binary and served from /assets/xterm, so this reaches nothing but the host
// the page itself came from. It happens once, when the page is first shown
// something to draw.
let loadXterm = () => {
  if (termAssets) return termAssets;
  const v = "?v=" + encodeURIComponent((schema && schema.version) || "0");
  const host = document.head || document.body;
  const once = (node, what) => new Promise((ok, no) => {
    node.addEventListener("load", () => ok());
    node.addEventListener("error", () => no(new Error("could not load " + what)));
    host.append(node);
  });
  termAssets = once(el("link", { rel: "stylesheet", href: "/assets/xterm/xterm.css" + v }), "the terminal styles")
    .then(() => once(el("script", { src: "/assets/xterm/xterm.js" + v }), "the terminal"))
    .then(() => once(el("script", { src: "/assets/xterm/addon-fit.js" + v }), "the fit addon"))
    .then(() => once(el("script", { src: "/assets/xterm/addon-unicode11.js" + v }), "the Unicode 11 addon"));
  return termAssets;
};

// makeTerminal is the one place xterm is named. What it returns is the whole
// of what this page asks of an emulator: show these bytes, tell me what was
// typed, be this size, be the size of your container, say what would fit,
// draw smaller, say how the arrow keys are to be sent, go away.
//
// It measures characters by Unicode 11, as a held session's own screen does
// (x/vt in the holder) and as scripts/vt/golden.mjs checks xterm against it:
// by xterm's own Unicode 6 widths an emoji such as ✅ takes one column where
// the holder gives it two, and every cell after it on that line would be
// drawn one column off. The addon is proposed API in xterm 5.5, so it has to
// be allowed.
let makeTerminal = async host => {
  await loadXterm();
  const term = new Terminal({
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
    fontSize: 13, scrollback: 10000, cursorBlink: true, convertEol: false,
    allowProposedApi: true,
    theme: { background: "#000000", foreground: "#e9e8e3", cursor: "#e9e8e3" },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.loadAddon(new Unicode11Addon.Unicode11Addon());
  term.unicode.activeVersion = "11";
  term.open(host);
  return {
    write: bytes => term.write(bytes),
    onData: cb => term.onData(cb),
    resize: (cols, rows) => { try { term.resize(cols, rows); } catch {} },
    fit: () => { try { fit.fit(); } catch {} return { cols: term.cols, rows: term.rows }; },
    // propose is what would fit the container at the current font, without
    // resizing to it; null when it cannot be measured, as while hidden.
    propose: () => {
      let p;
      try { p = fit.proposeDimensions(); } catch { return null; }
      return p && p.cols > 0 && p.rows > 0 ? { cols: p.cols, rows: p.rows } : null;
    },
    font: px => { try { term.options.fontSize = px; } catch {} },
    appCursor: () => !!(term.modes && term.modes.applicationCursorKeysMode),
    dispose: () => term.dispose(),
  };
};
