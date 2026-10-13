// Page behaviour, served as a file so the content security policy can
// refuse inline script. Controls are reached by delegation from document,
// so one file serves every page and htmx swaps need no rewiring.
'use strict';

// Theme: applied at once and remembered; the server renders it into the
// next page so there is no flash of the wrong colours.
document.addEventListener('change', function (e) {
  var pick = e.target.closest('[data-theme-pick]');
  if (!pick) return;
  var v = pick.value;
  var root = document.documentElement;
  if (v) { root.setAttribute('data-theme', v); } else { root.removeAttribute('data-theme'); }
  document.cookie = 'zs_theme=' + v + ';path=/;max-age=' + (v ? 31536000 : 0) + ';SameSite=Strict';
});

// Narrow screens: the sidenav and the topbar controls open behind one button.
document.addEventListener('click', function (e) {
  var t = e.target.closest('[data-nav-toggle]');
  if (!t) return;
  var open = document.body.classList.toggle('nav-open');
  t.setAttribute('aria-expanded', open ? 'true' : 'false');
});

// Ask before anything destructive. The question is an attribute, which
// keeps names out of a script string inside an HTML attribute.
document.addEventListener('submit', function (e) {
  var form = e.target.closest('form[data-confirm]');
  if (form && !window.confirm(form.getAttribute('data-confirm'))) e.preventDefault();
});
