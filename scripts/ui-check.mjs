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

async function evaluateAsync(expression) {
  const res = await send('Runtime.evaluate', {
    expression, returnByValue: true, awaitPromise: true,
  });
  if (res.exceptionDetails) {
    const d = res.exceptionDetails;
    throw new Error((d.exception && d.exception.description) || d.text);
  }
  return res.result.value;
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
  const titles = await evaluateAsync(`(async () => {
    const res = await fetch('api/nav', { cache: 'no-store' });
    const cfg = await res.json();
    return { configured: cfg.site.title, rendered: document.title };
  })()`);
  check('the page title came from the config',
    !!titles.configured && titles.rendered === titles.configured,
    `document.title=${JSON.stringify(titles.rendered)} config=${JSON.stringify(titles.configured)}`);

  /* ---------- tags and status lights must match the payload ---------- */

  // Both used to be decided in the browser and got it wrong: only the first two
  // tags were drawn while the search box matched all of them, and a link the
  // server never probes (ssh:, mailto:, probe: false) still grew a status light
  // stuck on "checking". Neither is visible to a Go test, so they are pinned
  // here, against the same payload the page was given.
  const linkView = await evaluateAsync(`(async () => {
    const cfg = await (await fetch('api/nav', { cache: 'no-store' })).json();
    const rendered = [...document.querySelectorAll('a.link')].map((a) => ({
      name: a.querySelector('.nm').textContent,
      tags: a.querySelectorAll('.tag').length,
      href: a.getAttribute('href'),
      hasDot: !!a.querySelector('.dot'),
    }));
    const links = (cfg.groups || []).flatMap((g) => g.links || []);
    return {
      links: links,
      rendered: rendered,
      // Pinned entries live in the strip, which never shows tags or a light.
      tagged: links.filter((l) => l.tags && l.tags.length && !l.pinned)
        .map((l) => ({ name: l.name, want: l.tags.length })),
      showDots: !!(cfg.status && cfg.status.enabled),
    };
  })()`);

  const tagMismatch = linkView.tagged
    .map((t) => {
      const row = linkView.rendered.find((r) => r.name === t.name);
      return row && row.tags !== t.want ? `${t.name}: rendered ${row.tags} of ${t.want}` : null;
    })
    .filter(Boolean);

  // The old bug drew at most two chips while the search box matched them all, so
  // this only proves anything when the config actually carries more than two and
  // the layout shows them. Say "not exercised" rather than pass silently: a green
  // check over a config that cannot fail is worse than no check.
  const tagShown = shape.layout === 'grid';
  const tagDiscriminating = linkView.tagged.some((t) => t.want > 2) && tagShown;
  if (!tagDiscriminating) {
    console.log('  skip every tag in the config is rendered — need a non-pinned link with >2'
      + ` tags in a layout that shows them (layout=${shape.layout}, widest=${Math.max(0, ...linkView.tagged.map((t) => t.want))})`);
  } else {
    check('every tag in the config is rendered', tagMismatch.length === 0, tagMismatch.join('; '));
  }

  const dotMismatch = linkView.rendered
    .map((row) => {
      const link = linkView.links.find((l) => l.url === row.href);
      if (!link) return null;
      // A light only ever appears for a link the server probes, and only when
      // probing is switched on at all.
      if (row.hasDot && !link.probeTarget) return `${row.name} has a light but is not probed`;
      return null;
    })
    .filter(Boolean);

  // This one catches the opposite failure too, so it only needs a config where
  // probing is on at all; without that there is no light to reason about.
  if (!linkView.showDots) {
    console.log('  skip no status light on a link the server never probes — status probing is'
      + ' switched off in this config, so nothing renders a light');
  } else {
    check('no status light on a link the server never probes',
      dotMismatch.length === 0, dotMismatch.join('; '));
  }

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

  /* ---------- a narrow viewport must collapse every layout to one column ---------- */

  // The per-layout rules are `:root[data-layout="…"] .group-grid`, which outranks
  // a bare `.group-grid` inside a media query. A phone rule written that way
  // silently loses for whichever layout declares its own columns -- dashboard
  // kept two cramped columns at 390px until this was fixed.
  const columnsAt = async (width, layout) => {
    await send('Emulation.setDeviceMetricsOverride', {
      width, height: 900, deviceScaleFactor: 1, mobile: true,
    });
    await evaluate(`document.documentElement.setAttribute('data-layout', ${JSON.stringify(layout)})`);
    await new Promise((r) => setTimeout(r, 200));
    return evaluate(
      `getComputedStyle(document.querySelector('.group-grid')).gridTemplateColumns.split(' ').filter(Boolean).length`);
  };

  // 600px, not a phone width: the point is to catch the media query losing to a
  // more specific rule. At 390px the dashboard columns do not fit side by side
  // anyway, so the layout collapses to one column on its own and the assertion
  // passes even when the rule is broken.
  for (const layout of ['grid', 'dashboard', 'list']) {
    const cols = await columnsAt(600, layout);
    check(`a 600px viewport collapses ${layout} to a single column`, cols === 1, `columns=${cols}`);
  }
  await send('Emulation.clearDeviceMetricsOverride');
  await evaluate(`document.documentElement.setAttribute('data-layout', ${JSON.stringify(shape.layout)})`);

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
