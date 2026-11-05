import assert from 'node:assert/strict';
const {chromium, webkit, devices} = await import(process.env.CUTTLE_PLAYWRIGHT_MODULE || 'playwright');
const remote = await chromium.connectOverCDP(process.env.CUTTLE_VIEWER_CDP_URL);
const browser = await webkit.launch();
const page = await remote.contexts()[0].newPage();
try {
  await page.goto('data:text/html;charset=utf-8,' + encodeURIComponent('<input id="text"><input id="pw" type="password"><input id="otp" autocomplete="one-time-code"><p style="height:200px">plain text</p>'));
  await page.bringToFront();
  await page.evaluate(() => {
    window.inputEvents = [];
    for (const type of ['keydown', 'input']) document.addEventListener(type, e => inputEvents.push({trusted: e.isTrusted, key: e.key}));
  });
  const cdp = await page.context().newCDPSession(page);
  async function listeners() {
    const {result} = await cdp.send('Runtime.evaluate', {expression: 'document'});
    const {listeners} = await cdp.send('DOMDebugger.getEventListeners', {objectId: result.objectId});
    return listeners.map(l => [l.type, l.useCapture, l.scriptId, l.lineNumber]);
  }
  const beforeListeners = await listeners();
  const before = await page.evaluate(() => Object.getOwnPropertyNames(window).sort());
  const viewer = await browser.newPage({...devices['iPhone 13']});
  await viewer.addInitScript(() => {
    window.EventSource = class extends EventSource {
      set onmessage(fn) {
        super.onmessage = e => {
          const state = JSON.parse(e.data);
          window.focusReport = state;
          if (window.hideFields && state) state.fields = [];
          fn({data: JSON.stringify(state)});
        };
      }
    };
    window.addEventListener('touchend', () => {
      const input = document.getElementById('cuttle-phone-keyboard');
      window.gestureKeyboard = {active: document.activeElement === input, mode: input?.inputMode};
    });
  });
  await viewer.goto(process.env.CUTTLE_VIEWER_URL);
  await viewer.waitForFunction(() => window.focusReport?.fields.length === 3 && window.webrtcInput);
  const input = viewer.locator('#cuttle-phone-keyboard');
  async function tapRect(rect) {
    const point = await viewer.evaluate(rect => {
      const bounds = document.getElementById('overlayInput').getBoundingClientRect();
      const canvas = document.getElementById('videoCanvas');
      return {x: bounds.x + (rect.x + rect.w / 2) * bounds.width / canvas.width,
        y: bounds.y + (rect.y + rect.h / 2) * bounds.height / canvas.height};
    }, rect);
    await viewer.touchscreen.tap(point.x, point.y);
    await viewer.waitForTimeout(150); // Selkies releases a touch click asynchronously.
  }
  const fields = await viewer.evaluate(() => window.focusReport.fields);
  await tapRect(fields[0].rect);
  assert.deepEqual(await viewer.evaluate(() => window.gestureKeyboard), {active: true, mode: 'text'});
  await page.waitForFunction(() => document.activeElement.id === 'text');
  await input.fill('Hello, cuttle! привіт');
  await page.waitForFunction(() => document.querySelector('#text').value === 'Hello, cuttle! привіт');
  await input.fill('Hello, cuttle! привіт!');
  await page.waitForFunction(() => document.querySelector('#text').value === 'Hello, cuttle! привіт!');
  await input.evaluate(el => {
    el.dispatchEvent(new CompositionEvent('compositionstart', {bubbles: true}));
    el.value = '\u200bComposed';
    el.dispatchEvent(new InputEvent('input', {bubbles: true, isComposing: true}));
  });
  assert.equal(await page.locator('#text').inputValue(), 'Hello, cuttle! привіт!');
  await input.evaluate(el => el.dispatchEvent(new CompositionEvent('compositionend', {bubbles: true})));
  await page.waitForFunction(() => document.querySelector('#text').value === 'Composed');
  await tapRect(fields[1].rect);
  await page.waitForFunction(() => document.activeElement.id === 'pw');
  assert.notEqual(await input.getAttribute('autocomplete'), 'current-password');
  await tapRect(fields[2].rect);
  await page.waitForFunction(() => document.activeElement.id === 'otp');
  assert.equal(await input.getAttribute('autocomplete'), 'one-time-code');
  await page.evaluate(() => document.activeElement.blur());
  await viewer.waitForFunction(() => document.querySelector('#cuttle-phone-keyboard').inputMode === 'none');
  await viewer.evaluate(() => { window.hideFields = true; });
  await viewer.waitForTimeout(500);
  await tapRect(fields[0].rect);
  assert.deepEqual(await viewer.evaluate(() => window.gestureKeyboard), {active: true, mode: 'none'});
  await viewer.waitForFunction(() => document.querySelector('#cuttle-phone-keyboard').inputMode === 'text');
  await tapRect({x: fields[0].rect.x, y: fields[0].rect.y + 100, w: 50, h: 30});
  assert.equal((await viewer.evaluate(() => window.gestureKeyboard)).mode, 'none');
  await viewer.waitForFunction(() => document.querySelector('#cuttle-phone-keyboard').inputMode === 'none');
  assert.deepEqual(await page.evaluate(() => Object.getOwnPropertyNames(window).sort()), before);
  assert.deepEqual(await listeners(), beforeListeners);
  const events = await page.evaluate(() => inputEvents);
  assert(events.length > 0 && events.every(e => e.trusted && !['CapsLock', 'NumLock'].includes(e.key)));
  await viewer.evaluate(() => { window.hideFields = false; });
  await page.evaluate(() => {
    const frame = document.createElement('iframe');
    frame.srcdoc = '<input id="child">';
    document.body.append(frame);
  });
  await viewer.waitForFunction(() => window.focusReport?.fields.length === 4);
  await tapRect(await viewer.evaluate(() => window.focusReport.fields[3].rect));
  await page.waitForFunction(() => document.querySelector('iframe').contentDocument.activeElement.id === 'child');
  const documentID = await viewer.evaluate(() => window.focusReport.document);
  await page.goto('data:text/html,<p>navigated</p>');
  await viewer.waitForFunction(id => window.focusReport?.document !== id && document.querySelector('#cuttle-phone-keyboard').inputMode === 'none', documentID);
  console.log('PASS: cached/unknown/plain taps, password/OTP, Unicode text/replacement, blur, iframe, navigation, trusted keys, main-world properties/listeners');
} finally {
  await page.close();
  await browser.close();
  await remote.close();
}
