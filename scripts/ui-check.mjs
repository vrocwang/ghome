#!/usr/bin/env node
/* Smoke-checks the navigation UI in a real browser.
 *
 * The frontend is a few thousand lines with no build step and, until now, no
 * test. Driving it headlessly has repeatedly caught bugs that reading the code
 * did not: a health pill that was never rendered, status dots stuck on
 * "checking", list-layout icons erasing themselves, and an empty-state message
 * that contradicted the pinned tile sitting right above it.
 *
 *   node scripts/ui-check.mjs http://127.0.0.1:8080/
 *
 * Exits non-zero on the first failed assertion. Needs Chrome; it is a
 * development tool, not part of the Go test suite, so `make test` stays
 * dependency free.
 */
import { spawn } from 'node:child_process';
import fs from 'node:fs';

const CHROME_CANDIDATES = [
  process.env.CHROME,
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
].filter(Boolean);

const url = process.argv[2];
if (!url) {
  console.error('usage: ui-check.mjs <url>');
  process.exit(2);
}

const chromePath = CHROME_CANDIDATES.find((p) => {
  try { return fs.statSync(p).isFile(); } catch { return false; }
});
if (!chromePath) {
  console.error('ui-check: no Chrome found; set $CHROME to its path');
  process.exit(2);
}

const port = 9400 + (process.pid % 300);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* ---------- tiny assertion runner ---------- */

let passed = 0;
const failures = [];

function check(name, ok, detail) {
  if (ok) {
    passed++;
    console.log(`  ok   ${name}`);
  } else {
    failures.push(name);
    console.log(`  FAIL ${name}${detail ? '  — ' + detail : ''}`);
  }
}

/* ---------- CDP plumbing ---------- */

let ws;
let nextId = 1;
const pending = new Map();
const exceptions = [];
const consoleErrors = [];
let iconMisses = 0;

function send(method, params = {}) {
  const id = nextId++;
  ws.send(JSON.stringify({ id, method, params }));
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    setTimeout(() => {
      if (pending.delete(id)) reject(new Error(method + ' timed out'));
    }, 20000);
  });
}

async function evaluate(expression) {
  const res = await send('Runtime.evaluate', { expression, returnByValue: true });
  if (res.exceptionDetails) {
    const d = res.exceptionDetails;
    throw new Error((d.exception && d.exception.description) || d.text);
  }
  return res.result.value;
}

const chrome = spawn(chromePath, [
  '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
  '--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage',
  '--remote-allow-origins=*', '--hide-scrollbars',
  `--remote-debugging-port=${port}`,
  `--user-data-dir=/tmp/ghome-ui-check-${port}`,
  '--window-size=1440,1100',
  'about:blank',
], { stdio: 'ignore' });

try {
  for (let i = 0; i < 150; i++) {
    try { if ((await fetch(`http://127.0.0.1:${port}/json/version`)).ok) break; } catch {}
    await sleep(120);
  }

  const target = await (await fetch(
    `http://127.0.0.1:${port}/json/new?${encodeURIComponent(url)}`, { method: 'PUT' })).json();
  ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => {
    ws.addEventListener('open', res, { once: true });
    ws.addEventListener('error', rej, { once: true });
  });
  ws.addEventListener('message', (event) => {
    const msg = JSON.parse(event.data);
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      msg.error ? reject(new Error(msg.error.message)) : resolve(msg.result);
      return;
    }
    if (msg.method === 'Runtime.exceptionThrown') {
      const d = msg.params.exceptionDetails;
      exceptions.push((d.exception && d.exception.description) || d.text);
    }
    if (msg.method === 'Log.entryAdded' && msg.params.entry.level === 'error') {
      const entry = msg.params.entry;
      // A 404 from /api/icon is a designed outcome, not a fault: it means the
      // site has no resolvable icon and the UI falls back to a letter avatar.
      // Any other failed resource is a real problem.
      if (entry.url && entry.url.includes('/api/icon')) {
        iconMisses++;
        return;
      }
      consoleErrors.push(entry.text + (entry.url ? ' (' + entry.url + ')' : ''));
    }
  });

  await send('Runtime.enable');
  await send('Log.enable');
  await sleep(4500); // config + status + stylesheet, with room for slow icons

  console.log(`ui-check ${url}\n`);

  /* ---------- did it render at all ---------- */

  const shape = await evaluate(`(() => {
    const q = (s) => document.querySelectorAll(s);
    const css = (p) => getComputedStyle(document.documentElement).getPropertyValue(p).trim();
    return {
      layout: document.documentElement.getAttribute('data-layout'),
      theme: document.documentElement.getAttribute('data-theme'),
      groups: q('.group').length,
      links: q('a.link').length,
      tiles: q('.tile').length,
      entries: q('a.link, a.tile').length,
      skeletons: q('.skeleton-card').length,
      stateShown: !!document.querySelector('#content > .state'),
      empty: (document.querySelector('.empty') || {}).textContent || null,
      accent: css('--accent'),
      radius: css('--radius'),
      title: document.title,
      hasApp: !!document.querySelector('.app'),
      themeHref: (document.getElementById('themeSheet') || { getAttribute: () => '' }).getAttribute('href') || '',
      missingIds: ['siteTitle','searchBar','searchInput','content','metaLine','themeToggle','banner','pinned','healthPill']
        .filter((id) => !document.getElementById(id)),
    };
  })()`);

  check('the page served the app shell', shape.hasApp,
    'no .app element: is this a ghome page?');
  check('no uncaught exceptions', exceptions.length === 0, exceptions[0]);
  check('no console errors', consoleErrors.length === 0, consoleErrors[0]);
  if (iconMisses) {
    console.log(`  note ${iconMisses} icon lookups returned 404 (expected when a link has no resolvable icon)`);
  }
  check('every element the app needs exists', shape.missingIds.length === 0, shape.missingIds.join(', '));
  check('the config rendered', shape.entries > 0, `entries=${shape.entries}`);
  check('no skeleton left behind', shape.skeletons === 0, `skeletons=${shape.skeletons}`);
  check('the loading state was replaced', !shape.stateShown);
  check('nothing reported as empty', !shape.empty, shape.empty);
  check('a layout is applied', ['grid', 'dashboard', 'list'].includes(shape.layout), shape.layout);
  check('a theme is applied', ['light', 'dark'].includes(shape.theme), shape.theme);
  check('the stylesheet is revision pinned', /\?v=/.test(shape.themeHref || ''), shape.themeHref);
  check('design tokens resolved', !!shape.accent && !!shape.radius, `${shape.accent} / ${shape.radius}`);
  check('the page title came from the config', !!shape.title && shape.title !== '导航', shape.title);

  /* ---------- pinned entries must not be duplicated ---------- */

  if (shape.tiles > 0) {
    const pinnedNames = await evaluate(`(() => {
      const inTiles = [...document.querySelectorAll('.tile .tile-name')].map(n => n.textContent);
      const inGroups = new Set([...document.querySelectorAll('a.link .nm')].map(n => n.textContent));
      return { duplicated: inTiles.filter(n => inGroups.has(n)) };
    })()`);
    check('pinned entries are not repeated inside groups',
      pinnedNames.duplicated.length === 0, pinnedNames.duplicated.join(', '));
  }

  /* ---------- search ---------- */

  const type = (q) => evaluate(`(() => {
    const i = document.getElementById('searchInput');
    if (!i) return { entries: 0, empty: '(no search box)' };
    i.value = ${JSON.stringify(q)};
    i.dispatchEvent(new Event('input', { bubbles: true }));
    return {
      entries: document.querySelectorAll('a.link, a.tile').length,
      empty: (document.querySelector('.empty') || {}).textContent || null,
    };
  })()`);

  const baseline = shape.entries;
  const nonsense = await type('zzz-definitely-no-match-zzz');
  check('a query matching nothing shows the empty state',
    nonsense.entries === 0 && !!nonsense.empty, JSON.stringify(nonsense));

  // The case that regressed: pinned entries live outside the groups, so a query
  // matching only a pinned one used to display it AND claim nothing matched.
  if (shape.tiles > 0) {
    const pinnedName = await evaluate(`(document.querySelector('.tile .tile-name')||{}).textContent || ''`);
    const onlyPinned = await type(pinnedName);
    check('a query matching only a pinned entry is not called empty',
      onlyPinned.entries > 0 && !onlyPinned.empty,
      `${JSON.stringify(pinnedName)} -> ${JSON.stringify(onlyPinned)}`);
  }

  const restored = await type('');
  check('clearing the query restores every entry',
    restored.entries === baseline && !restored.empty, JSON.stringify(restored));

  /* ---------- verdict ---------- */

  console.log('');
  if (failures.length) {
    console.log(`${passed} passed, ${failures.length} FAILED: ${failures.join(', ')}`);
    process.exitCode = 1;
  } else {
    console.log(`all ${passed} checks passed  (layout=${shape.layout}, ${shape.groups} groups, ${shape.entries} entries)`);
  }
} catch (err) {
  console.error('ui-check: ' + err.message);
  process.exitCode = 2;
} finally {
  try { ws?.close(); } catch {}
  chrome.kill('SIGKILL');
}
