/* ghome — single page navigation UI.
   No framework and no build step: the file is served straight from the Go
   binary. Everything is built with DOM APIs and textContent, never innerHTML,
   so a hostile nav.yaml cannot inject markup. */
(function () {
  'use strict';

  var POLL_INTERVAL = 60000;   // safety net when SSE is unavailable
  var RECONNECT_DELAY = 5000;  // after the SSE connection is closed
  var STALE_AFTER = 20000;     // refresh on tab focus if older than this
  var HUE_COUNT = 10;
  var MAX_BANNER_ITEMS = 4;

  var els = {
    content: document.getElementById('content'),
    title: document.getElementById('siteTitle'),
    subtitle: document.getElementById('siteSubtitle'),
    logo: document.getElementById('brandLogo'),
    searchBar: document.getElementById('searchBar'),
    searchInput: document.getElementById('searchInput'),
    searchHint: document.getElementById('searchHint'),
    footer: document.getElementById('siteFooter'),
    meta: document.getElementById('metaLine'),
    toast: document.getElementById('toast'),
    themeToggle: document.getElementById('themeToggle'),
    themeSheet: document.getElementById('themeSheet'),
    banner: document.getElementById('banner'),
    healthPill: document.getElementById('healthPill'),
    healthDot: document.getElementById('healthDot'),
    healthText: document.getElementById('healthText'),
    pinned: document.getElementById('pinned'),
    pinnedRow: document.getElementById('pinnedRow'),
    lockToggle: document.getElementById('lockToggle'),
    unlockForm: document.getElementById('unlockForm'),
    unlockToken: document.getElementById('unlockToken'),
    unlockCancel: document.getElementById('unlockCancel')
  };

  var stateBox = document.getElementById('stateBox');
  var stateText = document.getElementById('stateText');
  var retryButton = document.getElementById('retryButton');
  var stateDetail = document.createElement('p');
  stateDetail.className = 'state-detail';
  stateDetail.hidden = true;
  stateBox.appendChild(stateDetail);

  // Groups live in a host we can swap in and out, so the state box survives a
  // re-render.
  var gridHost = document.createElement('div');
  els.content.replaceChildren(gridHost);

  var state = {
    config: null,
    status: { enabled: false, results: {} },
    query: '',
    onlyProblems: false,
    visible: [],      // rendered <a> nodes, in DOM order, pinned first
    selected: -1,
    dotNodes: [],     // { url, el } so status updates can patch in place
    renderedRev: null,
    renderedThemeRev: null,
    lastFetch: 0,
    pollTimer: null,
    source: null,
    reconnectTimer: null,
    toastTimer: null
  };

  /* ---------- small helpers ---------- */

  function el(tag, className, text) {
    var node = document.createElement(tag);
    if (className) node.className = className;
    if (text != null) node.textContent = text;
    return node;
  }

  function hostOf(rawURL) {
    try {
      var u = new URL(rawURL, window.location.href);
      return (u.protocol === 'http:' || u.protocol === 'https:') ? u.hostname : null;
    } catch (e) {
      return null;
    }
  }

  function isImageURL(value) {
    return /^https?:\/\//i.test(value) || /^data:image\//i.test(value) || value.charAt(0) === '/';
  }

  // A short, whitespace-free string is treated as a glyph (an emoji, usually).
  function isGlyph(value) {
    return value.length > 0 && !/\s/.test(value) && Array.from(value).length <= 4;
  }

  function avatarFor(name) {
    var text = (name || '?').trim();
    var h = 0;
    for (var i = 0; i < text.length; i++) h = (h * 31 + text.charCodeAt(i)) >>> 0;
    return el('span', 'link-icon avatar hue-' + (h % HUE_COUNT),
      text.charAt(0).toUpperCase() || '?');
  }

  // Icons normally come from our own /api/icon, which resolves and caches the
  // site's real icon server side. That is what makes hashed icon paths and
  // internal hosts without a /favicon.ico still show something.
  function iconFor(link, revision) {
    var icon = (link.icon || '').trim();

    if (icon && icon !== 'auto') {
      if (isImageURL(icon)) return imageIcon(link, icon);
      if (isGlyph(icon)) return el('span', 'link-icon', icon);
    }

    var host = hostOf(link.url);
    if (host) return imageIcon(link, iconEndpoint(host, revision));
    return avatarFor(link.name);
  }

  function iconEndpoint(host, revision) {
    return 'api/icon?h=' + encodeURIComponent(host) +
      '&v=' + encodeURIComponent((revision || '').slice(0, 12));
  }

  function imageIcon(link, src) {
    var box = el('span', 'link-icon');
    var img = el('img');
    img.alt = '';
    img.loading = 'lazy';
    img.decoding = 'async';
    img.referrerPolicy = 'no-referrer';
    // Fall back to a letter avatar when nothing could be resolved.
    img.addEventListener('error', function () {
      if (box.parentNode) box.parentNode.replaceChild(avatarFor(link.name), box);
    });
    img.src = src;
    box.appendChild(img);
    return box;
  }

  function toast(message) {
    els.toast.textContent = message;
    els.toast.classList.add('show');
    if (state.toastTimer) clearTimeout(state.toastTimer);
    state.toastTimer = setTimeout(function () {
      els.toast.classList.remove('show');
    }, 2600);
  }

  function formatTime(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return '';
    var now = new Date();
    var hm = String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
    if (d.toDateString() === now.toDateString()) return hm;
    return (d.getMonth() + 1) + '-' + d.getDate() + ' ' + hm;
  }

  /* ---------- state box ---------- */

  function showState(title, detail, canRetry) {
    stateText.textContent = title;
    stateDetail.textContent = detail || '';
    stateDetail.hidden = !detail;
    retryButton.hidden = !canRetry;
    els.content.replaceChildren(stateBox);
  }

  function hideState() {
    if (gridHost.parentNode !== els.content) els.content.replaceChildren(gridHost);
  }

  // A skeleton grid reads as "content is coming" far better than a lone
  // spinner, and it costs nothing.
  function renderSkeletons() {
    var grid = el('div', 'skeleton-grid');
    for (var i = 0; i < 6; i++) {
      var card = el('div', 'skeleton-card');
      card.appendChild(el('div', 'sk-line sk-head'));
      for (var j = 0; j < 3; j++) {
        card.appendChild(el('div', 'sk-line' + (j === 2 ? ' sk-short' : '')));
      }
      grid.appendChild(card);
    }
    gridHost.replaceChildren(grid);
    hideState();
  }

  /* ---------- status ---------- */

  function statusFor(url) {
    if (!state.status || !state.status.enabled) return null;
    return (state.status.results || {})[url] || null;
  }

  function statusState(url) {
    var result = statusFor(url);
    if (result && result.state) return result.state;
    return state.status && state.status.enabled ? 'checking' : null;
  }

  function statusTitle(url) {
    var result = statusFor(url);
    if (!result) return state.status.enabled ? '正在探测…' : '';
    var parts = [];
    if (result.state === 'up') parts.push('在线');
    else if (result.state === 'down') parts.push('无法访问');
    else parts.push('状态未知');
    if (result.code) parts.push('HTTP ' + result.code);
    if (result.latencyMs) parts.push(result.latencyMs + ' ms');
    if (result.error) parts.push(result.error);
    if (result.checkedAt) parts.push('探测于 ' + formatTime(result.checkedAt));
    return parts.join(' · ');
  }

  function renderHealthPill() {
    var enabled = !!(state.status && state.status.enabled);
    els.healthPill.hidden = !enabled;
    if (!enabled) return;

    var results = state.status.results || {};
    var total = 0, up = 0, down = 0;
    Object.keys(results).forEach(function (url) {
      total++;
      if (results[url].state === 'up') up++;
      else if (results[url].state === 'down') down++;
    });

    var problems = total - up;
    els.healthText.textContent = total ? up + '/' + total + ' 在线' : '探测中…';
    els.healthDot.className = 'dot ' + (down > 0 ? 'down' : (problems > 0 ? 'unknown' : 'up'));
    els.healthPill.title = problems > 0
      ? problems + ' 个服务异常，点击只看异常'
      : '全部正常，点击只看异常';
    els.healthPill.setAttribute('aria-pressed', state.onlyProblems ? 'true' : 'false');
  }

  // Status updates patch the existing dots instead of re-rendering, so a probe
  // round every minute does not disturb scroll position or selection.
  function applyStatus() {
    state.dotNodes.forEach(function (entry) {
      var s = statusState(entry.url);
      entry.el.className = 'dot ' + (s || 'unknown');
      var title = statusTitle(entry.url);
      if (title) entry.el.title = title;
    });
    renderHealthPill();
    if (state.onlyProblems) renderContent();
  }

  /* ---------- rendering ---------- */

  function render() {
    var cfg = state.config;
    if (!cfg) return;
    renderHeader(cfg);
    renderLock();
    renderBanner(cfg);
    renderContent();
    renderHealthPill();
    renderMeta(cfg);
  }

  // The lock only appears when the config actually has private entries, so a nav
  // with none never grows a button that does nothing.
  function renderLock() {
    var view = (state.config && state.config.private) || null;
    if (!view || !view.total) {
      els.lockToggle.hidden = true;
      els.unlockForm.hidden = true;
      return;
    }
    els.lockToggle.hidden = false;
    els.lockToggle.classList.toggle('unlocked', !view.locked);
    els.lockToggle.title = view.locked
      ? '显示 ' + view.total + ' 个私密链接'
      : '已解锁 ' + view.total + ' 个私密链接，点此隐藏';
    if (!view.locked) els.unlockForm.hidden = true;
  }

  // The revision does not change when you unlock, so load() would decide there
  // is nothing to redraw. Clearing it forces a render. The result says whether
  // the view could be fetched at all: "still locked" is only a refusal when the
  // server actually answered.
  function reloadView() {
    state.renderedRev = null;
    return load({ notify: false });
  }

  function wireUnlock() {
    els.lockToggle.addEventListener('click', function () {
      var view = state.config && state.config.private;
      if (view && !view.locked) {
        setStoredToken('');
        reloadView();
        return;
      }
      els.unlockForm.hidden = !els.unlockForm.hidden;
      if (!els.unlockForm.hidden) els.unlockToken.focus();
    });

    els.unlockCancel.addEventListener('click', function () {
      els.unlockForm.hidden = true;
    });

    els.unlockForm.addEventListener('submit', async function (event) {
      event.preventDefault();
      var value = els.unlockToken.value.trim();
      if (!value) return;

      setStoredToken(value);
      els.unlockToken.value = '';
      var fetched = await reloadView();

      var view = state.config && state.config.private;
      if (view && view.locked && fetched) {
        // The forge refused it. Say so, rather than leaving the page looking
        // exactly as before, which reads as "the button is broken".
        setStoredToken('');
        await reloadView();
        els.unlockForm.hidden = false;
        els.unlockToken.focus();
        toast('令牌被拒绝：它不能读取配置仓');
        return;
      }
      if (view && view.locked) {
        // The request never got an answer (server restarting, network blip).
        // The token is kept: throwing it away here would make the user re-paste
        // a perfectly good one while debugging the wrong thing.
        els.unlockForm.hidden = false;
        els.unlockToken.focus();
        toast('暂时无法确认令牌，请重试');
        return;
      }
      els.unlockForm.hidden = true;
      toast('已解锁私密链接');
    });
  }

  function renderHeader(cfg) {
    var site = cfg.site || {};
    var title = site.title || '导航';
    document.title = title;
    els.title.textContent = title;

    els.subtitle.textContent = site.subtitle || '';
    els.subtitle.hidden = !site.subtitle;

    els.logo.replaceChildren();
    if (site.logo) {
      if (isImageURL(site.logo)) {
        var img = el('img');
        img.alt = '';
        img.src = site.logo;
        els.logo.appendChild(img);
      } else {
        els.logo.textContent = site.logo;
      }
      els.logo.hidden = false;
    } else {
      els.logo.hidden = true;
    }

    els.footer.textContent = site.footer || '';
    els.footer.hidden = !site.footer;

    var search = cfg.search || {};
    var engines = search.engines || [];
    els.searchBar.hidden = search.enabled === false;
    els.searchHint.textContent = search.hotkey || '/';
    els.searchInput.placeholder = engines.length ? '搜索，或按 / 聚焦…' : '搜索…';

    document.documentElement.setAttribute('data-layout', cfg.layout || 'grid');
  }

  // Warnings are non-fatal config problems (a typo'd key, an unusable colour).
  // Surfacing them here is what keeps "we tolerated it" from meaning "you never
  // find out".
  function renderBanner(cfg) {
    var warnings = (cfg.meta && cfg.meta.warnings) || [];
    if (!warnings.length) {
      els.banner.hidden = true;
      els.banner.replaceChildren();
      return;
    }

    els.banner.replaceChildren();
    els.banner.appendChild(el('span', 'banner-icon', '⚠'));

    var body = el('div');
    var head = el('div');
    head.appendChild(el('strong', null, '配置有 ' + warnings.length + ' 处被忽略，页面其余部分正常'));
    body.appendChild(head);

    var list = el('ul');
    warnings.slice(0, MAX_BANNER_ITEMS).forEach(function (warning) {
      list.appendChild(el('li', null, warning));
    });
    if (warnings.length > MAX_BANNER_ITEMS) {
      list.appendChild(el('li', null, '还有 ' + (warnings.length - MAX_BANNER_ITEMS) + ' 处，详见 /healthz'));
    }
    body.appendChild(list);
    els.banner.appendChild(body);
    els.banner.hidden = false;
  }

  function renderContent() {
    var cfg = state.config;
    var tokens = state.query.toLowerCase().split(/\s+/).filter(Boolean);
    var revision = cfg.meta && cfg.meta.revision;

    state.visible = [];
    state.dotNodes = [];
    state.selected = -1;

    // Pinned links are lifted out of the groups, so they are not part of
    // `shown`. Counting only group links made a query that matches just a
    // pinned entry report "nothing matched" while showing that entry.
    var pinned = renderPinned(cfg, tokens, revision);

    var grid = el('div', 'group-grid');
    var shown = 0;

    (cfg.groups || []).forEach(function (group) {
      var links = (group.links || []).filter(function (link) {
        if (link.pinned) return false; // pinned links live in the strip above
        return matches(link, tokens);
      });
      if (!links.length) return;
      shown += links.length;

      var card = el('section', 'group');
      if (group.color) card.style.setProperty('--group-accent', group.color);

      var head = el('div', 'group-head');
      if (group.icon) head.appendChild(el('span', 'group-icon', group.icon));
      head.appendChild(el('span', 'group-name', group.name));

      var meta = el('span', 'group-meta');
      if (group.description) meta.appendChild(el('span', 'group-desc', group.description));
      meta.appendChild(el('span', 'group-count', String(links.length)));
      head.appendChild(meta);
      card.appendChild(head);

      var list = el('div', 'links');
      links.forEach(function (link) {
        var node = renderLink(link, revision);
        state.visible.push(node);
        list.appendChild(node);
      });
      card.appendChild(list);
      grid.appendChild(card);
    });

    if (!shown && !pinned) grid.appendChild(el('div', 'empty', emptyMessage()));

    gridHost.replaceChildren(grid);
    hideState();
  }

  function emptyMessage() {
    if (state.onlyProblems) {
      return state.query ? '没有既异常又匹配「' + state.query + '」的服务' : '没有异常的服务';
    }
    if (state.query) return '没有匹配「' + state.query + '」的链接';
    return '配置里还没有链接';
  }

  function renderPinned(cfg, tokens, revision) {
    var pinned = [];
    (cfg.groups || []).forEach(function (group) {
      (group.links || []).forEach(function (link) {
        if (link.pinned && matches(link, tokens)) pinned.push(link);
      });
    });

    if (!pinned.length) {
      els.pinned.hidden = true;
      els.pinnedRow.replaceChildren();
      return 0;
    }

    els.pinnedRow.replaceChildren();
    pinned.forEach(function (link) {
      var a = el('a', 'tile');
      applyLinkTarget(a, link);
      a.title = link.description ? link.name + ' — ' + link.description : link.name;
      a.appendChild(iconFor(link, revision));
      a.appendChild(el('span', 'tile-name', link.name));
      var tileDot = statusDot(link);
      if (tileDot) a.appendChild(tileDot);
      els.pinnedRow.appendChild(a);
      state.visible.push(a);
    });
    els.pinned.hidden = false;
    return pinned.length;
  }

  function renderLink(link, revision) {
    var a = el('a', 'link');
    applyLinkTarget(a, link);
    a.title = link.description ? link.name + ' — ' + link.description : link.name;

    a.appendChild(iconFor(link, revision));

    var body = el('span', 'link-body');
    var name = el('span', 'link-name');
    name.appendChild(el('span', 'nm', link.name));
    if (link.hotkey) name.appendChild(el('kbd', 'hotkey', link.hotkey));
    body.appendChild(name);
    if (link.description) body.appendChild(el('span', 'link-desc', link.description));
    a.appendChild(body);

    if (link.tags && link.tags.length) {
      // Every tag is rendered: the cap belongs on the server, where exceeding
      // it produces a warning, rather than here where it silently dropped chips
      // that the search box still matched.
      var tags = el('span', 'link-tags');
      link.tags.forEach(function (tag) {
        tags.appendChild(el('span', 'tag', tag));
      });
      a.appendChild(tags);
    }

    var dot = statusDot(link);
    if (dot) a.appendChild(dot);
    return a;
  }

  function applyLinkTarget(a, link) {
    a.href = link.url;
    // Default to a new tab: a nav page is a launchpad, not a destination.
    if (link.newTab !== false) {
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
    }
  }

  // Only a link the server will actually probe gets a dot. The server answers
  // this in link.probeTarget because "there is no result for this URL" is
  // ambiguous on its own: it means both "not probed yet" (show "checking") and
  // "cannot be probed" (show nothing). Deciding it here as well is how a mailto:
  // entry ended up with a light that spun forever.
  function probeable(link) {
    return link.probeTarget === true;
  }

  function statusDot(link) {
    if (!state.status || !state.status.enabled) return null;
    if (!probeable(link)) return null;
    var wrap = el('span', 'link-status');
    var dot = el('span', 'dot ' + (statusState(link.url) || 'checking'));
    var title = statusTitle(link.url);
    if (title) dot.title = title;
    wrap.appendChild(dot);
    state.dotNodes.push({ url: link.url, el: dot });
    return wrap;
  }

  function renderMeta(cfg) {
    var meta = cfg.meta || {};
    var parts = [];
    if (meta.linkCount) parts.push(meta.linkCount + ' 个链接');
    if (meta.groupCount) parts.push(meta.groupCount + ' 个分组');
    if (meta.updatedAt) parts.push('更新于 ' + formatTime(meta.updatedAt));
    els.meta.textContent = parts.join(' · ');
    els.meta.title = meta.revision ? 'revision ' + meta.revision : '';
  }

  function matches(link, tokens) {
    if (state.onlyProblems) {
      // A link nobody probes has no state to be wrong: without this an ssh: or
      // mailto: entry -- which can never be "up" -- lands in the problems view.
      if (probeable(link)) {
        var s = statusState(link.url);
        if (s === 'up') return false;
      }
    }
    if (!tokens.length) return true;
    var haystack = [
      link.name,
      link.description || '',
      (link.tags || []).join(' '),
      link.url
    ].join(' ').toLowerCase();
    for (var i = 0; i < tokens.length; i++) {
      if (haystack.indexOf(tokens[i]) === -1) return false;
    }
    return true;
  }

  /* ---------- keyboard selection ---------- */

  function select(index) {
    if (state.selected >= 0 && state.visible[state.selected]) {
      state.visible[state.selected].classList.remove('selected');
    }
    state.selected = index;
    var node = state.visible[index];
    if (node) {
      node.classList.add('selected');
      node.scrollIntoView({ block: 'nearest' });
    }
  }

  function move(delta) {
    var n = state.visible.length;
    if (!n) return;
    if (state.selected < 0) {
      select(delta > 0 ? 0 : n - 1);
      return;
    }
    select((state.selected + delta + n) % n);
  }

  function openSelected() {
    var node = state.visible[state.selected >= 0 ? state.selected : 0];
    if (node) node.click();
  }

  function isTyping(target) {
    if (!target) return false;
    var tag = target.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || target.isContentEditable;
  }

  function focusSearch() {
    if (els.searchBar.hidden) return;
    els.searchInput.focus();
    els.searchInput.select();
  }

  /* ---------- web search ---------- */

  function engines() {
    var search = (state.config && state.config.search) || {};
    return search.engines || [];
  }

  // "g some words" uses the engine whose keyword is "g".
  function keywordMatch(query) {
    var m = /^(\S+)\s+([\s\S]+)$/.exec(query.trim());
    if (!m) return null;
    var list = engines();
    for (var i = 0; i < list.length; i++) {
      if (list[i].keyword && list[i].keyword.toLowerCase() === m[1].toLowerCase()) {
        return { engine: list[i], query: m[2] };
      }
    }
    return null;
  }

  function webSearch(query) {
    var list = engines();
    if (!list.length || !query.trim()) return false;
    var match = keywordMatch(query);
    var engine = match ? match.engine : list[0];
    var term = match ? match.query : query;
    window.open(engine.url.split('{q}').join(encodeURIComponent(term)), '_blank', 'noopener');
    return true;
  }

  /* ---------- theme ---------- */

  function cycleTheme() {
    var theme = window.ghomeTheme;
    if (!theme) return;
    var pref = theme.stored();
    var next = pref === '' ? 'light' : (pref === 'light' ? 'dark' : '');
    theme.set(next);
    updateThemeLabel();
    toast('主题：' + themeLabel());
  }

  function themeLabel() {
    var theme = window.ghomeTheme;
    var pref = theme ? theme.stored() : '';
    return pref === '' ? '跟随系统' : (pref === 'light' ? '浅色' : '深色');
  }

  function updateThemeLabel() {
    els.themeToggle.title = '主题：' + themeLabel() + '（点击切换，快捷键 T）';
  }

  // The stylesheet is generated from the config plus the repository's
  // custom.css, so a push that changes either has to bust the browser's copy.
  function applyThemeSheet(marker) {
    if (!els.themeSheet) return;
    els.themeSheet.setAttribute('href', 'theme.css?v=' + encodeURIComponent(marker || '0'));
  }

  /* ---------- data loading ---------- */

  // The private tier is unlocked with a Forgejo token that can read the config
  // repository. It lives in sessionStorage rather than a cookie: nothing is
  // attached automatically, so there is no CSRF surface, and closing the tab is
  // the logout.
  var TOKEN_KEY = 'ghome.token';

  function storedToken() {
    try { return sessionStorage.getItem(TOKEN_KEY) || ''; } catch (e) { return ''; }
  }

  function setStoredToken(value) {
    try {
      if (value) sessionStorage.setItem(TOKEN_KEY, value);
      else sessionStorage.removeItem(TOKEN_KEY);
    } catch (e) { /* private mode: the unlock simply will not persist */ }
    // Whoever is asking has changed, so what was fetched for the previous
    // identity is not ours to keep. Dropping the probe results matters most on
    // logout: if the next /api/status request failed, the old ones would keep a
    // private service's state in the health pill.
    state.status = { enabled: false, results: {} };
  }

  async function fetchJSON(path) {
    var headers = { Accept: 'application/json' };
    var token = storedToken();
    // Only these two answers depend on who is asking, and a token never belongs
    // in a URL.
    if (token && (path === 'api/nav' || path === 'api/status')) {
      headers.Authorization = 'Bearer ' + token;
    }
    var res = await fetch(path, { cache: 'no-store', headers: headers });
    var body = null;
    try {
      body = await res.json();
    } catch (e) {
      body = null;
    }
    if (!res.ok) {
      var message = (body && body.error) || ('HTTP ' + res.status);
      if (body && body.detail) message += '：' + body.detail;
      throw new Error(message);
    }
    return body;
  }

  async function load(options) {
    options = options || {};

    var navError = null;
    var navPromise = fetchJSON('api/nav').then(function (body) {
      state.config = body;
    }).catch(function (err) {
      navError = err;
    });

    // Status is optional: a failure there must never break the page.
    var statusPromise = fetchJSON('api/status').then(function (body) {
      if (body && typeof body.enabled === 'boolean') state.status = body;
    }).catch(function () { /* leave the previous status in place */ });

    await Promise.all([navPromise, statusPromise]);
    state.lastFetch = Date.now();

    if (navError) {
      if (state.config) {
        if (!options.silent) toast('刷新失败：' + navError.message);
      } else {
        showState('导航配置尚未就绪，正在重试…', navError.message, true);
      }
      return false;
    }

    var meta = state.config.meta || {};
    var revision = meta.revision;
    var themeRevision = meta.themeRevision || revision;

    // The stylesheet is tracked separately from the document on purpose: a
    // push that only touches custom.css leaves the config revision untouched,
    // so gating the stylesheet on it would leave a live page showing stale
    // styling until somebody reloaded.
    var themeChanged = themeRevision !== state.renderedThemeRev;
    var navChanged = revision !== state.renderedRev;

    if (themeChanged) {
      state.renderedThemeRev = themeRevision;
      applyThemeSheet(themeRevision);
    }

    if (navChanged) {
      state.renderedRev = revision;
      var scrollY = window.scrollY;
      render();
      window.scrollTo(0, scrollY);
      if (options.notify) toast('导航已更新');
    } else {
      applyStatus();
      if (options.notify && themeChanged) toast('样式已更新');
    }
    hideState();
    return true;
  }

  /* ---------- live updates ---------- */

  function startPolling() {
    if (state.pollTimer) return;
    state.pollTimer = setInterval(function () {
      load({ silent: true });
    }, POLL_INTERVAL);
  }

  function stopPolling() {
    if (state.pollTimer) {
      clearInterval(state.pollTimer);
      state.pollTimer = null;
    }
  }

  function scheduleReconnect() {
    if (state.reconnectTimer) return;
    state.reconnectTimer = setTimeout(function () {
      state.reconnectTimer = null;
      connect();
    }, RECONNECT_DELAY);
  }

  function connect() {
    // Start the safety net now, and stop it only once a stream is actually
    // established. Waiting for the stream to fail first is not enough: a
    // connection that never completes leaves EventSource retrying forever, and
    // a reconnect every few seconds keeps resetting the poll timer, so the page
    // ends up with neither events nor polls and never notices a change.
    startPolling();

    if (!window.EventSource) return;
    if (state.source) state.source.close();

    var es = new EventSource('api/events');
    state.source = es;

    es.addEventListener('open', function () {
      stopPolling();
    });

    // The server tells us which revision it is on, so a page that loaded in
    // the middle of a refresh can catch up.
    es.addEventListener('hello', function (event) {
      try {
        var payload = JSON.parse(event.data);
        var current = state.config && state.config.meta && state.config.meta.revision;
        if (payload.revision && payload.revision !== current) {
          load({ silent: true });
        }
      } catch (err) { /* ignore malformed frames */ }
    });

    // One event type means "re-read": config and probe results both live
    // behind it, so a dropped notification cannot leave one of them stale.
    es.addEventListener('changed', function () {
      load({ silent: true, notify: true });
    });

    es.addEventListener('error', function () {
      // startPolling is a no-op while a timer is already running, which is what
      // keeps a failing reconnect from pushing the next poll a further minute
      // away every five seconds.
      startPolling();
      if (es.readyState === 2) { // CLOSED: the browser will not retry for us
        scheduleReconnect();
      }
    });
  }

  /* ---------- events ---------- */

  function bindEvents() {
    els.searchInput.addEventListener('input', function () {
      state.query = els.searchInput.value;
      if (state.config) renderContent();
    });

    els.searchInput.addEventListener('keydown', function (event) {
      switch (event.key) {
        case 'ArrowDown':
          event.preventDefault();
          move(1);
          break;
        case 'ArrowUp':
          event.preventDefault();
          move(-1);
          break;
        case 'Enter':
          event.preventDefault();
          if ((event.metaKey || event.ctrlKey) && webSearch(state.query)) return;
          if (keywordMatch(state.query) && webSearch(state.query)) return;
          if (state.visible.length) openSelected();
          else webSearch(state.query);
          break;
        case 'Escape':
          event.preventDefault();
          if (els.searchInput.value) {
            els.searchInput.value = '';
            state.query = '';
            if (state.config) renderContent();
          } else {
            els.searchInput.blur();
          }
          break;
      }
    });

    document.addEventListener('keydown', function (event) {
      if (isTyping(event.target)) return;
      if (event.metaKey || event.ctrlKey || event.altKey) {
        if (event.key.toLowerCase() === 'k') {
          event.preventDefault();
          focusSearch();
        }
        return;
      }

      var key = event.key;

      if (key === 'Escape') {
        select(-1);
        return;
      }
      if (key === 'ArrowDown' || key === 'ArrowUp') {
        event.preventDefault();
        move(key === 'ArrowDown' ? 1 : -1);
        return;
      }
      if (key === 'Enter') {
        if (state.selected >= 0) {
          event.preventDefault();
          openSelected();
        }
        return;
      }
      if (key.length !== 1) return;

      // A link's own hotkey wins over the global ones, so a config can claim
      // any key it likes.
      var lower = key.toLowerCase();
      for (var i = 0; i < state.visible.length; i++) {
        var node = state.visible[i];
        var hotkey = node.querySelector('.hotkey');
        if (hotkey && hotkey.textContent.toLowerCase() === lower) {
          event.preventDefault();
          node.click();
          return;
        }
      }

      var searchHotkey = (state.config && state.config.search && state.config.search.hotkey) || '/';
      if (key === searchHotkey) {
        event.preventDefault();
        focusSearch();
        return;
      }
      if (lower === 't') {
        event.preventDefault();
        cycleTheme();
      }
    });

    retryButton.addEventListener('click', function () {
      showState('重新加载中…', '', false);
      load({ silent: false });
    });

    els.healthPill.addEventListener('click', function () {
      state.onlyProblems = !state.onlyProblems;
      if (state.config) renderContent();
      renderHealthPill();
    });

    els.themeToggle.addEventListener('click', cycleTheme);

    // Come back to a stale tab and it catches up immediately.
    document.addEventListener('visibilitychange', function () {
      if (document.visibilityState === 'visible' && Date.now() - state.lastFetch > STALE_AFTER) {
        load({ silent: true });
      }
    });
    window.addEventListener('online', function () {
      load({ silent: true });
    });
  }

  /* ---------- init ---------- */

  function init() {
    bindEvents();
    wireUnlock();
    updateThemeLabel();
    renderSkeletons();
    load({ silent: false }).then(connect);
  }

  init();
})();
