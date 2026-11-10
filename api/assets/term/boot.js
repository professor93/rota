"use strict";

/* The terminal page starting: the last of its files, run once every other
 * one has been, because it is the only one that does anything as it loads.
 */

/* ================================================================= boot == */
// The holder's terminal is the size of the space it is in, so it is refitted
// when that space changes. Neither of these exists everywhere this script is
// run, and neither is worth failing over.
if (typeof ResizeObserver === "function") {
  try { new ResizeObserver(() => termFit()).observe($("#tscreen")); } catch {}
}
if (window && window.addEventListener) window.addEventListener("resize", () => termFit());

// What the shared page asks of whichever page it is on.
onReady = () => { enterTerminal(); };
onSignedOut = () => {
  termDetach(); termDrop(); termEventsStop();
  terms = []; termId = null; termGen++; termInfo = null; termStatus = "idle";
  termPolling = false;
  renderGate();
};

renderGate();
pageBoot("terminal");
