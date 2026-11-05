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
  let state = null, stale = false, heard = 0, tap = null, start = null, wait = 0, blurTimer = 0, typed = '', composing = false;
  const contains = (r, p) => r && p.x >= r.x && p.y >= r.y && p.x <= r.x + r.w && p.y <= r.y + r.h;
  // WebKit reloads the software keyboard on every inputmode write, even an
  // unchanged one, so only real changes are written.
  function configure(field) {
    const mode = !field ? 'none' : /^(text|decimal|numeric|tel|search|email|url)$/.test(field.inputmode) ? field.inputmode
      : {number: 'decimal', otp: 'numeric', email: 'email', url: 'url', tel: 'tel', search: 'search'}[field.kind] || 'text';
    const autocomplete = field?.kind === 'otp' ? 'one-time-code' : 'off';
    const hint = field?.enterkeyhint || (field?.kind === 'search' ? 'search' : 'enter');
    if (input.inputMode !== mode) input.inputMode = mode;
    if (input.autocomplete !== autocomplete) input.autocomplete = autocomplete;
    if (input.enterKeyHint !== hint) input.enterKeyHint = hint;
  }
  function reset() {
    clearTimeout(wait); clearTimeout(blurTimer);
    wait = 0; blurTimer = 0; tap = null; typed = ''; composing = false; input.value = sentinel; configure(null); input.blur();
  }
  // A refused connection (non-200) closes an EventSource for good, so it is
  // reopened after a pause; dropped streams reconnect on their own.
  function connect() {
    const source = new EventSource(new URL('api/cuttle-focus', document.baseURI));
    source.onerror = () => {
      state = null; reset();
      if (source.readyState === EventSource.CLOSED) setTimeout(connect, 3000);
    };
    source.onmessage = receive;
  }
  function receive(event) {
    const previous = state;
    state = JSON.parse(event.data); heard = performance.now(); stale = false;
    if (!state || previous && (previous.target !== state.target || previous.document !== state.document)) { reset(); return; }
    // A field can move or grow on focus, or a label can focus one elsewhere, so
    // a newly focused field confirms the tap as well as one under it.
    if (tap && state.focus && (contains(state.focus.rect, tap) || state.focus.id === tap.field || state.focus.id !== tap.from)) {
      clearTimeout(wait); wait = 0; tap = null; configure(state.focus);
    } else if (!tap && document.activeElement === input) {
      if (state.focus) {
        if (previous?.focus?.id !== state.focus.id) { typed = ''; input.value = sentinel; }
        clearTimeout(blurTimer); blurTimer = 0; configure(state.focus); }
      else if (!blurTimer) blurTimer = setTimeout(reset, 300);
    }
  }
  connect();
  document.addEventListener('touchstart', event => {
    if (!event.target.closest('#overlayInput')) return;
    clearTimeout(wait); tap = null;
    // Two fingers scroll or zoom the remote page, which moves every cached field.
    if (event.touches.length > 1) stale = true;
    start = event.touches.length === 1 ? {x: event.touches[0].clientX, y: event.touches[0].clientY, at: performance.now()} : null;
  }, {capture: true, passive: true});
  document.addEventListener('touchmove', event => {
    if (!start) return;
    const t = event.touches[0];
    if (!t || Math.hypot(t.clientX - start.x, t.clientY - start.y) > 10) { start = null; stale = true; }
  }, {capture: true, passive: true});
  document.addEventListener('touchend', event => {
    if (!start || event.touches.length || performance.now() - start.at > 350) { start = null; return; }
    start = null;
    // Any tap moves the remote caret, so the typed history no longer describes it.
    if (document.activeElement === input) { typed = ''; input.value = sentinel; }
    if (!state || performance.now() - heard > 1000) return;
    const pointer = window.webrtcInput;
    if (!pointer || pointer._trackpadMode) return;
    tap = {x: pointer.x, y: pointer.y, from: state.focus?.id};
    const field = stale ? undefined : state.fields.find(f => contains(f.rect, tap));
    tap.field = field?.id;
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
    // The caret can sit before the sentinel (hardware arrows, spacebar drag),
    // so it is removed wherever it is rather than only as a prefix.
    const kept = input.value.includes(sentinel);
    const next = input.value.replace(sentinel, '');
    const before = Array.from(typed), after = Array.from(next);
    let prefix = 0;
    while (prefix < before.length && prefix < after.length && before[prefix] === after[prefix]) prefix++;
    // Deleting the sentinel itself is a backspace past the typed text.
    for (let i = prefix; i < before.length + (!kept && !next && !before.length ? 1 : 0); i++) key(0xff08);
    for (const char of after.slice(prefix)) {
      const cp = char.codePointAt(0);
      key(cp === 10 ? 0xff0d : cp === 9 ? 0xff09 : cp >= 0x20 && cp <= 0xff ? cp : 0x01000000 + cp);
    }
    typed = next;
    if (!input.value.startsWith(sentinel)) input.value = sentinel + next;
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
  input.addEventListener('blur', () => { typed = ''; composing = false; input.value = sentinel; });
  // Above the daemon's 2 s sample bound, so one slow sample does not close the keyboard.
  setInterval(() => { if (performance.now() - heard > 3000) { state = null; reset(); } }, 1000);
  if (window.visualViewport) {
    const fit = () => {
      const overlay = document.getElementById('overlayInput');
      if (overlay) overlay.parentElement.style.maxHeight = visualViewport.height + 'px';
    };
    visualViewport.addEventListener('resize', fit); fit();
  }
})();
