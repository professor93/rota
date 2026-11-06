// golden.mjs feeds a capture to @xterm/headless and prints what is on its
// screen afterwards, as the JSON the holder's screen tests compare x/vt with.
//
//   XTERM_HEADLESS=<dir>/node_modules/@xterm/headless \
//     node scripts/vt/golden.mjs <bin> <cols> <rows> > <name>.xterm.json
//
// xterm is the reference because it is what the web page draws with. The
// package is never committed and never installed here: scripts/vt/README says
// how to fetch it, checked, into a directory of its own.
//
// The document has four parts:
//
//   rows    each row's text, trailing blanks trimmed
//   cells   each cell that shows something: a character that is not blank, or
//           a background, inverse or underline that a blank still shows
//   cursor  where the cursor is and whether it is hidden
//   modes   the input modes a viewer's keys and mouse depend on
//
// A colour is "default", "p<n>" for the palette or "#rrggbb". The second half
// of a wide character is not a cell of its own. A cursor waiting to wrap past
// the last column is reported in that column, where both emulators keep it.
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const [file, cols, rows] = process.argv.slice(2);
if (!file || !(cols > 0) || !(rows > 0)) {
  console.error('usage: node golden.mjs <bin> <cols> <rows>');
  process.exit(2);
}
const pkg = process.env.XTERM_HEADLESS;
if (!pkg) {
  console.error('golden.mjs: set XTERM_HEADLESS to the @xterm/headless 5.5.0 directory; see scripts/vt/README');
  process.exit(2);
}
const { Terminal } = createRequire(import.meta.url)(pkg);

const term = new Terminal({ cols: +cols, rows: +rows, allowProposedApi: true, scrollback: 0 });
await new Promise((done) => term.write(readFileSync(file), done));

function colour(mode, palette, rgb, value) {
  if (mode) return 'default';
  if (palette) return 'p' + value;
  if (rgb) return '#' + value.toString(16).padStart(6, '0');
  return 'default';
}

const buf = term.buffer.active;
const doc = { rows: [], cells: [], cursor: null, modes: null };
for (let y = 0; y < term.rows; y++) {
  const line = buf.getLine(buf.baseY + y);
  // translateToString(true) trims only cells nothing was written to; a space
  // that was written is a blank as well, and x/vt cannot tell the two apart.
  doc.rows.push(line ? line.translateToString(true).replace(/ +$/, '') : '');
  if (!line) continue;
  for (let x = 0; x < term.cols; x++) {
    const c = line.getCell(x);
    if (!c || c.getWidth() === 0) continue;
    const ch = c.getChars() || ' ';
    const cell = {
      x, y, ch,
      fg: colour(c.isFgDefault(), c.isFgPalette(), c.isFgRGB(), c.getFgColor()),
      bg: colour(c.isBgDefault(), c.isBgPalette(), c.isBgRGB(), c.getBgColor()),
      bold: !!c.isBold(),
      italic: !!c.isItalic(),
      underline: !!c.isUnderline(),
      inverse: !!c.isInverse(),
      dim: !!c.isDim(),
    };
    if (ch !== ' ' || cell.bg !== 'default' || cell.inverse || cell.underline) doc.cells.push(cell);
  }
}
doc.cursor = {
  x: Math.min(buf.cursorX, term.cols - 1),
  y: buf.cursorY,
  hidden: !!term._core.coreService.isCursorHidden,
};
const mouse = { none: 0, x10: 9, vt200: 1000, drag: 1002, any: 1003 };
doc.modes = {
  alt: buf.type === 'alternate',
  cursorKeys: term.modes.applicationCursorKeysMode,
  bracketedPaste: term.modes.bracketedPasteMode,
  mouse: mouse[term.modes.mouseTrackingMode],
  focus: term.modes.sendFocusMode,
};

// One row and one cell to a line, so a changed golden reads as a diff.
const out = ['{', ' "rows": ['];
doc.rows.forEach((r, i) => out.push('  ' + JSON.stringify(r) + (i < doc.rows.length - 1 ? ',' : '')));
out.push(' ],', ' "cells": [');
doc.cells.forEach((c, i) => out.push('  ' + JSON.stringify(c) + (i < doc.cells.length - 1 ? ',' : '')));
out.push(' ],');
out.push(' "cursor": ' + JSON.stringify(doc.cursor) + ',');
out.push(' "modes": ' + JSON.stringify(doc.modes));
out.push('}');
process.stdout.write(out.join('\n') + '\n');
