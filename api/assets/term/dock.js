"use strict";

/* The dock under the screen, for the composer and the quick keys. Until it
 * has them it is hidden, and the strip has no toggle for it.
 */

// renderTermDock draws the dock, which for now is nothing to draw.
function renderTermDock() {
  hide($("#tdock"), true);
}

// termDockToggle is the strip's way to show or hide the dock. Nothing yet.
function termDockToggle() { return null; }
