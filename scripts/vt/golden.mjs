// golden.mjs feeds a capture to @xterm/headless and prints what is on its
// screen afterwards, as the JSON the holder's screen tests compare x/vt with.
//
//   XTERM_HEADLESS=<dir>/node_modules/@xterm/headless \
//   XTERM_UNICODE11=<dir>/node_modules/@xterm/addon-unicode11 \
//     node scripts/vt/golden.mjs <bin> <cols> <rows> > <name>.xterm.json
//
// xterm is the reference because it is what the web page draws with. It
// measures characters by Unicode 11 through @xterm/addon-unicode11 0.8.0, so
// an emoji such as ✅ is two columns wide, as in real terminals and in x/vt;
// xterm's own default, Unicode 6, makes it one. Neither package is ever
// committed or installed here: scripts/vt/README says how to fetch them,
// checked, into a directory of their own.
//
// The document has five parts:
//
//   sha256  the sha256 of the bytes it was made from, so a test can tell a
//           golden made from another capture or another repaint
//   rows    each row's text, trailing blanks trimmed
//   cells   each cell that shows something: a character that is not blank, or
//           a background, inverse or underline that a blank still shows
//   cursor  where the cursor is and whether it is hidden
//   modes   the input modes a viewer's keys and mouse depend on
//
// A colour is "default", "p<n>" for the palette or "#rrggbb". The second half
// of a wide character is not a cell of its own. A cursor waiting to wrap past
// the last column is reported in that column, where both emulators keep it.
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const [file, cols, rows] = process.argv.slice(2);
if (!file || !(cols > 0) || !(rows > 0)) {
  console.error('usage: node golden.mjs <bin> <cols> <rows>');
  process.exit(2);
}
const pkg = process.env.XTERM_HEADLESS;
const u11 = process.env.XTERM_UNICODE11;
if (!pkg || !u11) {
  console.error('golden.mjs: set XTERM_HEADLESS to the @xterm/headless 5.5.0 directory and ' +
    'XTERM_UNICODE11 to the @xterm/addon-unicode11 0.8.0 one; see scripts/vt/README');
  process.exit(2);
}
const load = createRequire(import.meta.url);
const { Terminal } = load(pkg);
const { Unicode11Addon } = load(u11);

const term = new Terminal({ cols: +cols, rows: +rows, allowProposedApi: true, scrollback: 0 });
term.loadAddon(new Unicode11Addon());
term.unicode.activeVersion = '11';
if (term.unicode.activeVersion !== '11') {
  console.error('golden.mjs: xterm did not take Unicode 11 widths');
  process.exit(1);
}
const bin = readFileSync(file);
await new Promise((done) => term.write(bin, done));

function colour(mode, palette, rgb, value) {
  if (mode) return 'default';
  if (palette) return 'p' + value;
  if (rgb) return '#' + value.toString(16).padStart(6, '0');
  return 'default';
}

const buf = term.buffer.active;
const doc = { sha256: createHash('sha256').update(bin).digest('hex'), rows: [], cells: [], cursor: null, modes: null };
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
const out = ['{', ' "sha256": ' + JSON.stringify(doc.sha256) + ',', ' "rows": ['];
doc.rows.forEach((r, i) => out.push('  ' + JSON.stringify(r) + (i < doc.rows.length - 1 ? ',' : '')));
out.push(' ],', ' "cells": [');
doc.cells.forEach((c, i) => out.push('  ' + JSON.stringify(c) + (i < doc.cells.length - 1 ? ',' : '')));
out.push(' ],');
out.push(' "cursor": ' + JSON.stringify(doc.cursor) + ',');
out.push(' "modes": ' + JSON.stringify(doc.modes));
out.push('}');
process.stdout.write(out.join('\n') + '\n');
