// SPIKE-ONLY oracle: feeds a raw byte stream to @xterm/headless and dumps a
// JSON fingerprint in the same canonical shape as Go's term.TakeFingerprint,
// so the two implementations can be diffed cell-by-cell.
//
// Usage: node oracle.mjs <fixture-file> [--cols N] [--rows N]
//            [--resize c:r,c:r...] [--snapshot N] [--post <file>]
//
//   --snapshot N   serialize viewport (scrollback=N; -1 = all) and replay
//                  into a fresh terminal, then fingerprint THAT terminal.
//   --resize a:b,c:d  apply resize sequence before fingerprinting.
//   --post FILE    write FILE's bytes after the main stream (continuation).

import { readFileSync } from 'node:fs';
import headlessPkg from '@xterm/headless';
import serializePkg from '@xterm/addon-serialize';

const { Terminal } = headlessPkg;
const { SerializeAddon } = serializePkg;

const argv = process.argv.slice(2);
if (argv.length === 0) {
  console.error('usage: node oracle.mjs <fixture> [--cols N] [--rows N] [--resize c:r,...] [--snapshot N] [--post f]');
  process.exit(2);
}
const fixture = argv[0];
let cols = 80, rows = 24, resizeSeq = [], snapshotSb = null, postFile = null;
for (let i = 1; i < argv.length; i++) {
  if (argv[i] === '--cols') cols = parseInt(argv[++i], 10);
  else if (argv[i] === '--rows') rows = parseInt(argv[++i], 10);
  else if (argv[i] === '--resize') resizeSeq = argv[++i].split(',').map((s) => s.split(':').map(Number));
  else if (argv[i] === '--snapshot') snapshotSb = parseInt(argv[++i], 10);
  else if (argv[i] === '--post') postFile = argv[++i];
}

const data = readFileSync(fixture);

function makeTerm() {
  return new Terminal({ cols, rows, allowProposedApi: true, scrollback: 1000 });
}

function write(term, buf) {
  return new Promise((resolve) => term.write(buf.toString('utf8'), resolve));
}

const FG_BG = (isRgb, isPal, get) =>
  isRgb() ? 'rgb:' + get().toString(16).padStart(6, '0') : isPal() ? 'pal:' + get() : 'def';

function cellString(cell) {
  let chars = cell.getChars();
  if (chars === '') chars = '·';
  let flags = '';
  if (cell.isBold()) flags += 'b';
  if (cell.isDim()) flags += 'd';
  if (cell.isItalic()) flags += 'i';
  if (cell.isUnderline()) flags += 'u';
  if (cell.isInverse()) flags += 'v';
  if (cell.isBlink()) flags += 'k';
  if (cell.isInvisible()) flags += 'n';
  if (cell.isStrikethrough()) flags += 's';
  if (cell.isOverline()) flags += 'o';
  const fg = FG_BG(() => cell.isFgRGB(), () => cell.isFgPalette(), () => cell.getFgColor());
  const bg = FG_BG(() => cell.isBgRGB(), () => cell.isBgPalette(), () => cell.getBgColor());
  return `${chars}|${cell.getWidth()}|${fg}|${bg}|${flags}`;
}

function fingerprint(term) {
  const buffer = term.buffer.active;
  const cells = [];
  const nullCell = buffer.getNullCell();
  for (let y = 0; y < term.rows; y++) {
    const line = buffer.getLine(buffer.viewportY + y);
    const row = [];
    for (let x = 0; x < term.cols; x++) {
      row.push(cellString(line ? line.getCell(x, nullCell) ?? nullCell : nullCell));
    }
    cells.push(row);
  }
  const m = term.modes;
  const modes =
    `ins=${m.insertMode} appcur=${m.applicationCursorKeysMode} appkey=${m.applicationKeypadMode} ` +
    `brpaste=${m.bracketedPasteMode} origin=${m.originMode} rewrap=${m.reverseWraparoundMode} ` +
    `sendfocus=${m.sendFocusMode} wrap=${m.wraparoundMode} mouse=${String(m.mouseTrackingMode).toUpperCase()} ` +
    `mousenc=? sync=${m.synchronizedOutputMode}`;
  return {
    cols: term.cols,
    rows: term.rows,
    altActive: buffer.type === 'alternate',
    cursorX: buffer.cursorX,
    cursorY: buffer.cursorY,
    cursorHidden: 'n/a',
    scrollTop: 'n/a',
    scrollBottom: 'n/a',
    ybase: buffer.baseY,
    linesTotal: buffer.length,
    modes,
    curAttr: 'n/a',
    cells,
    normalScrollback: term.buffer.normal.length - term.rows,
  };
}

const term = makeTerm();
await write(term, data);
if (postFile) await write(term, readFileSync(postFile));
for (const [c, r] of resizeSeq) term.resize(c, r);

let target = term;
if (snapshotSb !== null) {
  const addon = new SerializeAddon();
  term.loadAddon(addon);
  const opts = snapshotSb < 0 ? {} : { scrollback: snapshotSb };
  const snap = addon.serialize(opts);
  target = makeTerm();
  await write(target, '\x1b[0m\x1b[H\x1b[2J\x1b[3J\x1b[H' + snap);
}

console.log(JSON.stringify(fingerprint(target)));
term.dispose();
if (target !== term) target.dispose();
