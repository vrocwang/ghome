/* Resolves the theme before the first paint so there is no flash of the wrong
   one. Kept as a separate synchronous script because the server sends a strict
   Content-Security-Policy that forbids inline scripts. */
(function () {
  'use strict';

  var KEY = 'ghome.theme'; // '' means follow the system
  var mq = window.matchMedia('(prefers-color-scheme: dark)');

  function stored() {
    try {
      return localStorage.getItem(KEY) || '';
    } catch (e) {
      return ''; // private mode, or storage disabled
    }
  }

  function resolve(pref) {
    if (pref === 'light' || pref === 'dark') return pref;
    return mq.matches ? 'dark' : 'light';
  }

  function apply() {
    var pref = stored();
    var root = document.documentElement;
    root.setAttribute('data-theme', resolve(pref));
    root.setAttribute('data-theme-pref', pref || 'auto');
  }

  function set(pref) {
    try {
      if (pref) localStorage.setItem(KEY, pref);
      else localStorage.removeItem(KEY);
    } catch (e) { /* ignore */ }
    apply();
  }

  apply();

  // Follow the system while the user has not made an explicit choice.
  if (mq.addEventListener) {
    mq.addEventListener('change', function () {
      if (!stored()) apply();
    });
  }

  // Only what the page actually calls: apply() already ran above, and resolve
  // and the storage key are implementation details of this file.
  window.ghomeTheme = { apply: apply, set: set, stored: stored };
})();
