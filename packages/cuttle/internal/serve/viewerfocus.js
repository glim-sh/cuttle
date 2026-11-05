function (geometry) {
  if (document.visibilityState !== 'visible' || !document.hasFocus()) return null;
  const {bounds, viewport} = JSON.parse(geometry);
  const scale = viewport.zoom || 1;
  const offsetX = bounds.left / scale;
  const offsetY = (bounds.top + bounds.height) / scale - window.innerHeight;
  const rect = (x, y, w, h) => ({x: x * scale, y: y * scale, w: w * scale, h: h * scale});
  const identities = globalThis.cuttleFocusIDs ||= {map: new WeakMap(), next: 0};
  const fields = [];
  let visited = 0;
  let focus = null;
  function scan(doc, dx, dy, clip) {
    for (const el of doc.querySelectorAll('input, textarea, [contenteditable], iframe')) {
      if (++visited > 2000 || fields.length >= 500) break;
      const b = el.getBoundingClientRect();
      const x = dx + b.x, y = dy + b.y;
      if (!b.width || !b.height || x + b.width <= clip.x || y + b.height <= clip.y || x >= clip.x + clip.w || y >= clip.y + clip.h) continue;
      const left = Math.max(x, clip.x), top = Math.max(y, clip.y);
      const right = Math.min(x + b.width, clip.x + clip.w), bottom = Math.min(y + b.height, clip.y + clip.h);
      const hitTest = () => doc.elementFromPoint((left + right) / 2 - dx, (top + bottom) / 2 - dy);
      if (el.tagName === 'IFRAME') {
        if (hitTest() !== el) continue;
        let transformed = false;
        for (let parent = el; parent; parent = parent.parentElement) {
          if (doc.defaultView.getComputedStyle(parent).transform !== 'none') { transformed = true; break; }
        }
        if (transformed) continue;
        try {
          if (el.contentDocument) scan(el.contentDocument, x + el.clientLeft, y + el.clientTop,
            {x: left, y: top, w: right - left, h: bottom - top});
        } catch {}
        continue;
      }
      if (el.matches(':disabled') || el.readOnly || doc.defaultView.getComputedStyle(el).visibility !== 'visible') continue;
      let kind = el.tagName === 'TEXTAREA' ? 'textarea' : el.isContentEditable ? 'contenteditable' : el.type;
      if (!['text','email','number','search','url','tel','password','textarea','contenteditable'].includes(kind)) continue;
      if ((el.autocomplete || '').split(/\s+/).includes('one-time-code')) kind = 'otp';
      if (!identities.map.has(el)) identities.map.set(el, ++identities.next);
      const field = {id: identities.map.get(el), kind, inputmode: el.inputMode, enterkeyhint: el.enterKeyHint,
        rect: rect(offsetX + left, offsetY + top, right - left, bottom - top)};
      const hit = hitTest();
      if (hit === el || el.contains(hit)) fields.push(field);
      if (doc.hasFocus() && doc.activeElement === el) focus = field;
    }
  }
  scan(document, 0, 0, {x: 0, y: 0, w: innerWidth, h: innerHeight});
  return {document: performance.timeOrigin, fields, focus};
}
