// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. https://mozilla.org/MPL/2.0/.
(() => {
  if (!/iPhone|iPod|Android.+Mobile/.test(navigator.userAgent)) return;
  const input = document.createElement('textarea');
  input.id = 'cuttle-phone-keyboard';
  input.classList.add('allow-native-input');
  input.style.cssText = 'position:fixed;left:0;top:0;width:1px;height:1px;opacity:0.01;font-size:16px;padding:0;border:0;pointer-events:none';
  input.autocapitalize = 'off';
  input.spellcheck = false;
  input.setAttribute('autocorrect', 'off');
  document.body.append(input);
  const sentinel = '\u200b';
  input.value = sentinel;
  let state = null, heard = 0, tap = null, start = null, wait = 0, blurTimer = 0, typed = '', composing = false;
  const contains = (r, p) => r && p.x >= r.x && p.y >= r.y && p.x <= r.x + r.w && p.y <= r.y + r.h;
  function configure(field) {
    input.inputMode = field ? ({number: 'decimal', otp: 'numeric', email: 'email', url: 'url', tel: 'tel', search: 'search'}[field.kind] || 'text') : 'none';
    if (field && /^(text|decimal|numeric|tel|search|email|url)$/.test(field.inputmode)) input.inputMode = field.inputmode;
    input.autocomplete = field?.kind === 'otp' ? 'one-time-code' : 'off';
    input.enterKeyHint = field?.enterkeyhint || (field?.kind === 'search' ? 'search' : 'enter');
  }
  function reset() {
    clearTimeout(wait); clearTimeout(blurTimer);
    wait = 0; blurTimer = 0; tap = null; typed = ''; input.value = sentinel; configure(null); input.blur();
  }
  const source = new EventSource(new URL('api/cuttle-focus', document.baseURI));
  source.onerror = () => { state = null; reset(); };
  source.onmessage = event => {
    const previous = state;
    state = JSON.parse(event.data); heard = performance.now();
    if (!state || previous && (previous.target !== state.target || previous.document !== state.document)) { reset(); return; }
    if (tap && state.focus && contains(state.focus.rect, tap)) {
      clearTimeout(wait); wait = 0; tap = null; configure(state.focus);
    } else if (!tap && document.activeElement === input) {
      if (state.focus) {
        if (previous?.focus?.id !== state.focus.id) { typed = ''; input.value = sentinel; }
        clearTimeout(blurTimer); blurTimer = 0; configure(state.focus); }
      else if (!blurTimer) blurTimer = setTimeout(reset, 300);
    }
  };
  document.addEventListener('touchstart', event => {
    if (!event.target.closest('#overlayInput')) return;
    clearTimeout(wait); tap = null;
    start = event.touches.length === 1 ? {x: event.touches[0].clientX, y: event.touches[0].clientY, at: performance.now()} : null;
  }, {capture: true, passive: true});
  document.addEventListener('touchmove', event => {
    if (!start) return;
    const t = event.touches[0];
    if (!t || Math.hypot(t.clientX - start.x, t.clientY - start.y) > 10) { start = null; state = null; }
  }, {capture: true, passive: true});
  document.addEventListener('touchend', event => {
    if (!start || event.touches.length || performance.now() - start.at > 350) { start = null; return; }
    start = null;
    const overlay = event.target.closest('#overlayInput');
    if (!overlay || !state || performance.now() - heard > 1000) return;
    const pointer = window.webrtcInput;
    if (!pointer || pointer._trackpadMode) return;
    tap = {x: pointer.x, y: pointer.y};
    const field = state.fields.find(f => contains(f.rect, tap));
    clearTimeout(blurTimer); blurTimer = 0;
    typed = ''; input.value = sentinel; configure(field);
    input.focus({preventScroll: true});
    clearTimeout(wait); wait = setTimeout(reset, 1000);
  }, {passive: true});
  function key(sym) {
    const sender = window.webrtcInput;
    if (!sender) return;
    sender.send('kd,' + sym); sender.send('ku,' + sym);
  }
  function edit() {
    const kept = input.value.startsWith(sentinel);
    const next = kept ? input.value.slice(1) : input.value;
    const before = Array.from(typed), after = Array.from(next);
    let prefix = 0;
    while (prefix < before.length && prefix < after.length && before[prefix] === after[prefix]) prefix++;
    for (let i = prefix; i < before.length + (!kept && !next ? 1 : 0); i++) key(0xff08);
    for (const char of after.slice(prefix)) {
      const cp = char.codePointAt(0);
      key(cp === 10 ? 0xff0d : cp <= 255 ? cp : 0x01000000 + cp);
    }
    typed = next;
    if (!kept) { input.value = sentinel + next; }
    if (typed.length > 1000) { typed = ''; input.value = sentinel; }
  }
  for (const name of ['keydown', 'keyup', 'keypress', 'beforeinput', 'input', 'compositionstart', 'compositionupdate', 'compositionend', 'paste']) {
    input.addEventListener(name, event => {
      event.stopPropagation();
      if (name === 'compositionstart') composing = true;
      if (name === 'compositionend') { composing = false; edit(); }
      if (name === 'input' && !composing && !event.isComposing) edit();
    });
  }
  input.addEventListener('blur', () => { typed = ''; input.value = sentinel; });
  setInterval(() => { if (performance.now() - heard > 1500) { state = null; reset(); } }, 1000);
  if (window.visualViewport) {
    const fit = () => {
      const overlay = document.getElementById('overlayInput');
      if (overlay) overlay.parentElement.style.maxHeight = visualViewport.height + 'px';
    };
    visualViewport.addEventListener('resize', fit); fit();
  }
})();
