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
      // Pinned entries live in the strip, which shows no tags but does show a
      // status light (and its hotkey).
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

  // Both directions: a light on something the server never probes, and a
  // probeable link that lost its light. Only the first was reported before, so a
  // regression that dropped every light passed.
  const dotMismatch = linkView.links
    .map((link) => {
      const row = linkView.rendered.find((r) => r.name === link.name);
      if (!row) return null;
      const want = linkView.showDots && link.probeTarget === true;
      if (row.hasDot === want) return null;
      return want
        ? `${link.name} should have a light but has none`
        : `${link.name} has a light but is not probed`;
    })
    .filter(Boolean);

  if (!linkView.showDots) {
    console.log('  skip status lights match the payload — status probing is'
      + ' switched off in this config, so nothing renders a light');
  } else {
    check('status lights match the payload',
      dotMismatch.length === 0, dotMismatch.join('; '));
  }

  /* ---------- the health pill counts every target, not just the answers so far ---------- */

  // Its denominator decides whether the pill says "全部正常" over a set of
  // services nobody has answered for yet.
  const pill = await evaluate(`(() => {
    const el = document.getElementById('healthText');
    const dot = document.getElementById('healthDot');
    return { text: el ? el.textContent : null, dot: dot ? dot.className : null };
  })()`);
  if (!linkView.showDots) {
    console.log('  skip the health pill counts every target — probing is off in this config');
  } else {
    const m = /^(\d+)\/(\d+)/.exec(pill.text || '');
    const up = m ? Number(m[1]) : 0;
    const shown = m ? Number(m[2]) : 0;
    const want = new Set(linkView.links.filter((l) => l.probeTarget === true).map((l) => l.url)).size;
    check('the health pill counts every target, not just the answers so far',
      shown === Math.max(want, 1) && up <= shown,
      `${JSON.stringify(pill.text)} for ${want} probe targets`);
    // One of the sample links points at a closed port, so a green dot here means
    // the pill is not looking at the results at all.
    if (want > 0) {
      check('the health pill is not green while a target has not answered',
        !/\bdot up\b/.test(' ' + pill.dot + ' '), pill.dot);
    }
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

    // Pinned entries take a second rendering path, and the global hotkey handler
    // finds a link's key by looking for this node: it used to be missing here, so
    // a hotkey on a pinned entry did nothing (and a pinned "t" hit the theme).
    const hotkeys = await evaluateAsync(`(async () => {
      const cfg = await (await fetch('api/nav', { cache: 'no-store' })).json();
      const want = (cfg.groups || []).flatMap((g) => g.links || [])
        .filter((l) => l.pinned && l.hotkey)
        .map((l) => ({ name: l.name, hotkey: l.hotkey }));
      const tiles = [...document.querySelectorAll('a.tile')].map((t) => ({
        name: (t.querySelector('.nm') || {}).textContent || '',
        hotkey: (t.querySelector('.hotkey') || {}).textContent || '',
      }));
      return { want: want, tiles: tiles };
    })()`);
    const hotkeyMismatch = hotkeys.want
      .map((w) => {
        const tile = hotkeys.tiles.find((t) => t.name === w.name);
        return tile && tile.hotkey.toLowerCase() === w.hotkey.toLowerCase()
          ? null : `${w.name}: tile shows ${JSON.stringify((tile || {}).hotkey || '')}`;
      })
      .filter(Boolean);
    if (!hotkeys.want.length) {
      console.log('  skip pinned hotkeys are rendered — no pinned entry in this config has one');
    } else {
      check('pinned hotkeys are rendered', hotkeyMismatch.length === 0, hotkeyMismatch.join('; '));
    }
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

  /* ---------- the theme toggle actually changes the page ---------- */

  // The toggle used to look broken: /theme.css emitted the config's light
  // colours under :root AND :root[data-theme="dark"], so every colour the
  // config named came back identical in dark mode and only the tokens it did
  // not mention moved. Asserting data-theme alone misses that entirely -- the
  // attribute changes, the page does not. So this compares what is actually
  // painted on both sides of a click. The contrast floor is the other half of
  // the promise: a theme that changes but cannot be read is not a theme.
  const paint = `(() => {
    const lum = (value) => {
      // The tokens are hex by default but any CSS colour is allowed, so accept
      // both spellings: hex first (its digits are not what a number scan finds),
      // then the rgb()/rgba() triple.
      const text = String(value).trim();
      let parts = null;
      const hex = text.match(/^#([0-9a-f]{6}|[0-9a-f]{3})$/i);
      if (hex) {
        const h = hex[1].length === 3 ? hex[1].replace(/./g, (c) => c + c) : hex[1];
        parts = [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16));
      } else {
        // [0-9] rather than \d: this whole probe is a template literal, and a
        // backslash-d inside one is not an escape at all -- the backslash is
        // dropped before the browser ever parses the regex, so \d silently
        // becomes "d" and an rgb() colour fails to parse.
        const nums = text.match(/[0-9.]+/g);
        if (nums && nums.length >= 3) parts = nums.slice(0, 3).map(Number);
      }
      if (!parts) return null;
      const [r, g, b] = parts.map((v) => {
        v /= 255;
        return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
      });
      return 0.2126 * r + 0.7152 * g + 0.0722 * b;
    };
    // Measure the text where it actually sits: link names are painted on the
    // card (--surface), not on the page background. Comparing --text against
    // --bg instead passed a config whose cards were #777777 on #808080.
    const cs = getComputedStyle(document.documentElement);
    const card = document.querySelector('.group') || document.body;
    const cardCS = getComputedStyle(card);
    const ratioOf = (a, b) => {
      const [x, y] = [lum(a), lum(b)];
      if (x === null || y === null) return 0;
      return Math.round(((Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)) * 100) / 100;
    };
    const name = document.querySelector('.link .nm') || document.querySelector('.link');
    const nameCS = name ? getComputedStyle(name) : null;
    return {
      theme: document.documentElement.getAttribute('data-theme'),
      bg: cs.getPropertyValue('--bg').trim(),
      // On the card, which is what a link name is read against...
      ratio: nameCS ? ratioOf(nameCS.color, cardCS.backgroundColor) : 0,
      // ...and on the page, which is what the rest of the chrome sits on.
      pageRatio: ratioOf(cardCS.color, cs.getPropertyValue('--bg')),
    };
  })()`;

  await send('Emulation.setEmulatedMedia', {
    features: [{ name: 'prefers-color-scheme', value: 'light' }],
  });
  await evaluate(`(() => { try { localStorage.removeItem('ghome.theme'); } catch (e) {} })()`);
  await send('Page.reload');
  await sleep(2500); // back to the config's own render

  const lightPaint = await evaluate(paint);
  await evaluate(`document.getElementById('themeToggle').click()`);
  await sleep(200);
  await evaluate(`document.getElementById('themeToggle').click()`);
  await sleep(300);
  const darkPaint = await evaluate(paint);

  check('the toggle reaches dark mode', darkPaint.theme === 'dark',
    `${lightPaint.theme} -> ${darkPaint.theme}`);
  check('dark mode repaints the page, not just the attribute',
    lightPaint.bg !== darkPaint.bg,
    `--bg stayed ${lightPaint.bg}; a style: block is feeding light colours to the dark theme`);
  check('both themes are readable on the cards', lightPaint.ratio >= 4.5 && darkPaint.ratio >= 4.5,
    `light ${lightPaint.ratio}:1, dark ${darkPaint.ratio}:1 (page ${lightPaint.pageRatio}:1 / ${darkPaint.pageRatio}:1)`);

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
