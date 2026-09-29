# SPDX: MIT
"""Behavioural probes shared by realref.py (real Chrome) and detect.py (ours).

One definition, run on both sides, so every value in posture.json compares like
with like. Dependency-free on purpose: the Windows reference box runs realref.py
with a bare Python and must not need anything installed.

A probe is one PROBES entry: the source of an async JS function whose resolved
value is JSON-serialisable. `run(conn)` serves a two-origin page on 127.0.0.1
(a secure context, so the gated APIs are live), navigates to it and evaluates
every probe there. `conn` is either script's CDP client; only `cmd(method,
params)` and `eval(expr, await_promise)` are used.

Nothing identifying is recorded: ports are rewritten to the origin labels A and
B, and WebRTC addresses are reduced to a class unless they are documentation
addresses (which is what the smoke passes as a fake exit IP).
"""
from __future__ import annotations

import json
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PROBE_TIMEOUT_MS = 25000  # under detect.py's 30s socket timeout

PROBES: dict[str, str] = {}

PROBES["canvas"] = r"""async () => {
  const h = d => { let v = 0x811c9dc5; for (let i = 0; i < d.length; i++) { v ^= typeof d === 'string' ? d.charCodeAt(i) : d[i]; v = Math.imul(v, 16777619) >>> 0; } return v.toString(16); };
  const TEXT = 'Cwm fjordbank glyphs vext quiz';
  const draw = c => { c.width = 220; c.height = 60; const x = c.getContext('2d'); x.textBaseline = 'top'; x.font = '18px Arial'; x.fillStyle = '#f60'; x.fillRect(10, 10, 100, 30); x.fillStyle = '#069'; x.fillText(TEXT, 4, 20); x.strokeStyle = 'rgba(102,204,0,0.7)'; x.beginPath(); x.arc(170, 30, 20, 0, Math.PI * 2); x.stroke(); return x; };
  const c = document.createElement('canvas'); const x = draw(c);
  const urls = [c.toDataURL(), c.toDataURL(), c.toDataURL()].map(h);
  const imgs = [0, 1, 2].map(() => h(x.getImageData(0, 0, c.width, c.height).data));
  const mt = () => { const m = x.measureText(TEXT); return [m.width, m.actualBoundingBoxLeft, m.actualBoundingBoxRight, m.actualBoundingBoxAscent, m.actualBoundingBoxDescent, m.fontBoundingBoxAscent, m.fontBoundingBoxDescent]; };
  const m1 = mt(), m2 = mt();
  // CreepJS's two lie triggers, in its context state: a fresh default-font context.
  const c2 = document.createElement('canvas'); c2.width = c2.height = 2; const x2 = c2.getContext('2d');
  const e = x2.measureText('');
  const empty = [e.actualBoundingBoxAscent, e.actualBoundingBoxDescent, e.actualBoundingBoxLeft, e.actualBoundingBoxRight, e.fontBoundingBoxAscent, e.fontBoundingBoxDescent];
  x2.fillStyle = '#000'; x2.fillRect(0, 0, 2, 2); x2.fillStyle = '#fff'; x2.fillRect(2, 2, 1, 1);
  x2.beginPath(); x2.arc(0, 0, 2, 0, 1, true); x2.closePath(); x2.fill();
  const lowEntropy = x2.getImageData(0, 0, 2, 2).data.join('');
  const c8 = document.createElement('canvas'); c8.width = c8.height = 8; const x8 = c8.getContext('2d');
  x8.fillStyle = '#08f'; x8.fillRect(0, 0, 8, 8); x8.clearRect(0, 0, 8, 8);
  const cleared = Math.max(...x8.getImageData(0, 0, 8, 8).data);
  const asUrl = b => new Promise(r => { const f = new FileReader(); f.onload = () => r(f.result); f.readAsDataURL(b); });
  const blob = await asUrl(await new Promise(r => c.toBlob(r, 'image/png')));
  const oc = new OffscreenCanvas(220, 60); draw(oc);
  const off = await asUrl(await oc.convertToBlob({type: 'image/png'}));
  const d = document.createElement('div');
  d.style.cssText = 'position:absolute;left:0;top:0;width:100px;height:100px;transform:rotate(45deg)';
  document.body.appendChild(d); const r = d.getClientRects()[0]; d.remove();
  return {
    toDataURL: urls[0], getImageData: imgs[0], measureTextWidth: m1[0],
    stable: {toDataURL: new Set(urls).size === 1, getImageData: new Set(imgs).size === 1, measureText: m1.join() === m2.join()},
    clearRectMax: cleared,
    emptyMeasureIntegers: empty.every(v => Number.isInteger(v || 0)), emptyMeasure: empty, lowEntropyImageData: lowEntropy,
    toBlobMatches: h(blob) === urls[0], convertToBlobHash: h(off), convertToBlobMatchesToBlob: off === blob,
    rotatedRect: [r.x, r.y, r.width, r.height],
  };
}"""

PROBES["shader"] = r"""async () => {
  const gl = document.createElement('canvas').getContext('webgl');
  if (!gl) return {error: 'no webgl'};
  const ext = gl.getExtension('WEBGL_debug_shaders');
  const out = {ext: !!ext};
  if (ext) {
    const dialect = s => /metal_stdlib|metal::|\[\[position\]\]/.test(s) ? 'msl'
      : /float4|SV_Position|: POSITION|static float/.test(s) ? 'hlsl'
      : /#version|gl_Position/.test(s) ? 'glsl' : (s ? 'other' : 'empty');
    const src = {
      vertex: [gl.VERTEX_SHADER, 'attribute vec4 p; varying vec2 v; void main() { v = p.xy; gl_Position = p; }'],
      fragment: [gl.FRAGMENT_SHADER, 'precision mediump float; varying vec2 v; uniform float t; void main() { gl_FragColor = vec4(v, sin(t), 1.0); }'],
    };
    for (const [k, [type, code]] of Object.entries(src)) {
      const s = gl.createShader(type); gl.shaderSource(s, code); gl.compileShader(s);
      const t = ext.getTranslatedShaderSource(s) || '';
      out[k] = {dialect: dialect(t), len: t.length, head: t.slice(0, 200)};
    }
  }
  const lose = gl.getExtension('WEBGL_lose_context'); if (lose) lose.loseContext();
  return out;
}"""

PROBES["webgl_caps"] = r"""async () => {
  const P1 = ['VERSION', 'SHADING_LANGUAGE_VERSION', 'VENDOR', 'RENDERER',
    'MAX_TEXTURE_SIZE', 'MAX_CUBE_MAP_TEXTURE_SIZE', 'MAX_RENDERBUFFER_SIZE', 'MAX_VIEWPORT_DIMS',
    'MAX_VERTEX_ATTRIBS', 'MAX_VERTEX_UNIFORM_VECTORS', 'MAX_VARYING_VECTORS', 'MAX_FRAGMENT_UNIFORM_VECTORS',
    'MAX_TEXTURE_IMAGE_UNITS', 'MAX_VERTEX_TEXTURE_IMAGE_UNITS', 'MAX_COMBINED_TEXTURE_IMAGE_UNITS',
    'ALIASED_LINE_WIDTH_RANGE', 'ALIASED_POINT_SIZE_RANGE', 'RED_BITS', 'GREEN_BITS', 'BLUE_BITS',
    'ALPHA_BITS', 'DEPTH_BITS', 'STENCIL_BITS', 'SUBPIXEL_BITS', 'SAMPLES', 'SAMPLE_BUFFERS'];
  const P2 = P1.concat(['MAX_3D_TEXTURE_SIZE', 'MAX_ARRAY_TEXTURE_LAYERS', 'MAX_COLOR_ATTACHMENTS',
    'MAX_DRAW_BUFFERS', 'MAX_ELEMENT_INDEX', 'MAX_ELEMENTS_INDICES', 'MAX_ELEMENTS_VERTICES',
    'MAX_FRAGMENT_INPUT_COMPONENTS', 'MAX_FRAGMENT_UNIFORM_BLOCKS', 'MAX_FRAGMENT_UNIFORM_COMPONENTS',
    'MAX_PROGRAM_TEXEL_OFFSET', 'MIN_PROGRAM_TEXEL_OFFSET', 'MAX_SAMPLES', 'MAX_SERVER_WAIT_TIMEOUT',
    'MAX_TEXTURE_LOD_BIAS', 'MAX_TRANSFORM_FEEDBACK_INTERLEAVED_COMPONENTS',
    'MAX_TRANSFORM_FEEDBACK_SEPARATE_ATTRIBS', 'MAX_TRANSFORM_FEEDBACK_SEPARATE_COMPONENTS',
    'MAX_UNIFORM_BLOCK_SIZE', 'MAX_UNIFORM_BUFFER_BINDINGS', 'MAX_VARYING_COMPONENTS',
    'MAX_VERTEX_OUTPUT_COMPONENTS', 'MAX_VERTEX_UNIFORM_BLOCKS', 'MAX_VERTEX_UNIFORM_COMPONENTS',
    'MAX_COMBINED_UNIFORM_BLOCKS', 'MAX_COMBINED_FRAGMENT_UNIFORM_COMPONENTS',
    'MAX_COMBINED_VERTEX_UNIFORM_COMPONENTS', 'UNIFORM_BUFFER_OFFSET_ALIGNMENT', 'MAX_CLIENT_WAIT_TIMEOUT_WEBGL']);
  const val = v => (v && typeof v === 'object' && 'length' in v) ? Array.from(v) : v;
  const one = (kind, names) => {
    const gl = document.createElement('canvas').getContext(kind);
    if (!gl) return null;
    const params = {};
    for (const n of names) if (gl[n] !== undefined) params[n] = val(gl.getParameter(gl[n]));
    const dbg = gl.getExtension('WEBGL_debug_renderer_info');
    if (dbg) { params.UNMASKED_VENDOR_WEBGL = gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL); params.UNMASKED_RENDERER_WEBGL = gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL); }
    const an = gl.getExtension('EXT_texture_filter_anisotropic');
    if (an) params.MAX_TEXTURE_MAX_ANISOTROPY_EXT = gl.getParameter(an.MAX_TEXTURE_MAX_ANISOTROPY_EXT);
    const db = kind === 'webgl' && gl.getExtension('WEBGL_draw_buffers');
    if (db) { params.MAX_DRAW_BUFFERS_WEBGL = gl.getParameter(db.MAX_DRAW_BUFFERS_WEBGL); params.MAX_COLOR_ATTACHMENTS_WEBGL = gl.getParameter(db.MAX_COLOR_ATTACHMENTS_WEBGL); }
    const precision = {};
    for (const s of ['VERTEX_SHADER', 'FRAGMENT_SHADER'])
      for (const p of ['LOW_FLOAT', 'MEDIUM_FLOAT', 'HIGH_FLOAT', 'LOW_INT', 'MEDIUM_INT', 'HIGH_INT']) {
        const f = gl.getShaderPrecisionFormat(gl[s], gl[p]);
        precision[s + '.' + p] = f && [f.rangeMin, f.rangeMax, f.precision];
      }
    const out = {params, precision, extensions: (gl.getSupportedExtensions() || []).slice().sort(), attributes: gl.getContextAttributes()};
    const lose = gl.getExtension('WEBGL_lose_context'); if (lose) lose.loseContext();
    return out;
  };
  return {webgl: one('webgl', P1), webgl2: one('webgl2', P2)};
}"""

PROBES["webgpu"] = r"""async () => {
  if (!navigator.gpu) return {gpu: false};
  const a = await Promise.race([navigator.gpu.requestAdapter(), new Promise(r => setTimeout(() => r('timeout'), 8000))]);
  const base = {gpu: true, preferredCanvasFormat: navigator.gpu.getPreferredCanvasFormat(),
    wgslLanguageFeatures: [...(navigator.gpu.wgslLanguageFeatures || [])].sort()};
  if (a === 'timeout' || !a) return {...base, adapter: a || null};
  const i = a.info || {};
  const limits = {}; for (const k in a.limits) limits[k] = a.limits[k];
  return {...base, adapter: {isFallbackAdapter: i.isFallbackAdapter ?? a.isFallbackAdapter ?? null,
    info: {vendor: i.vendor, architecture: i.architecture, device: i.device, description: i.description,
      subgroupMinSize: i.subgroupMinSize, subgroupMaxSize: i.subgroupMaxSize},
    features: [...a.features].sort(), limits}};
}"""

PROBES["webrtc"] = r"""async () => {
  const P = window.__probe;
  const cls = a => {
    if (!a) return a;
    if (a.endsWith('.local')) return 'mdns';
    if (a === '0.0.0.0' || /^(203\.0\.113|198\.51\.100|192\.0\.2)\./.test(a)) return a;
    if (a.includes(':')) return /^(fe80|fc|fd|::1)/i.test(a) ? 'ipv6-private' : 'ipv6-public';
    return /^(10\.|127\.|169\.254\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.|100\.(6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.)/.test(a) ? 'ipv4-private' : 'ipv4-public';
  };
  const gather = async (servers, ms) => {
    const pc = new RTCPeerConnection({iceServers: servers}); const c = [];
    pc.onicecandidate = e => e.candidate && e.candidate.candidate && c.push(e.candidate.candidate);
    pc.createDataChannel('p'); await pc.setLocalDescription(await pc.createOffer());
    await new Promise(r => { const t = setTimeout(r, ms); pc.onicegatheringstatechange = () => { if (pc.iceGatheringState === 'complete') { clearTimeout(t); r(); } }; });
    const state = pc.iceGatheringState; pc.close();
    return {state, candidates: c.map(s => { const f = s.split(' '); const ra = f.indexOf('raddr');
      return {type: f[7], protocol: f[2], address: cls(f[4]), raddr: ra > 0 ? cls(f[ra + 1]) : null, rport: ra > 0 ? f[ra + 3] : null}; })};
  };
  const stun = await gather([{urls: 'stun:stun.l.google.com:19302'}], 6000);
  const local = await gather([{urls: `stun:${P.udpHost}:${P.udpPort}`}], 3000);
  await new Promise(r => setTimeout(r, 500));
  const udp = (await (await fetch('/udp', {cache: 'no-store'})).json()).packets;
  return {...stun, types: [...new Set(stun.candidates.map(c => c.type))].sort(),
    hostIsMdns: stun.candidates.filter(c => c.type === 'host').every(c => c.address === 'mdns'),
    localStunPackets: udp, localStunTypes: local.candidates.map(c => c.type)};
}"""

# issue #50, patch 0056. Each count is how often a page getter fired around one
# console call, on the page and in a worker. Real Chrome with no debugger reads
# errorName 1, regexpFlag 1, tableColumn 1, nodeListLength 0; with Runtime
# enabled, unpatched V8 previews each argument and adds reads. run() evaluates it
# again with Runtime enabled; detect.py also runs it driver-shaped (auto-attach).
CDP_GETTER_PROBE = r"""async () => {
  const probe = () => {
    const reads = (target, key, run) => {
      const saved = Object.getOwnPropertyDescriptor(target, key);
      let n = 0;
      Object.defineProperty(target, key, {configurable: true, get() {
        n++;
        return saved && ("value" in saved ? saved.value : saved.get.call(this));
      }});
      try { run(); } finally {
        if (saved) Object.defineProperty(target, key, saved); else delete target[key];
      }
      return n;
    };
    const columns = [];
    const out = {
      errorName: reads(Error.prototype, "name", () => console.debug(new Error(""))),
      regexpFlag: reads(RegExp.prototype, "global", () => console.debug(/x/g)),
      tableColumn: reads(columns, 0, () => console.table([{a: 1}], columns)),
    };
    if (typeof NodeList !== "undefined") {
      out.nodeListLength = reads(NodeList.prototype, "length",
                                 () => console.debug(document.querySelectorAll("p")));
    }
    return out;
  };
  const src = "self.onmessage = () => self.postMessage((" + probe + ")())";
  const w = new Worker(URL.createObjectURL(new Blob([src], {type: "text/javascript"})));
  const worker = await new Promise((resolve) => {
    w.onmessage = (e) => resolve(e.data);
    w.onerror = (e) => resolve({error: String(e.message)});
    setTimeout(() => resolve({error: "worker timeout"}), 8000);
    w.postMessage(0);
  });
  w.terminate();
  return {page: probe(), worker};
}"""
PROBES["cdp"] = CDP_GETTER_PROBE

# dabi hasInconsistentWorkerValues compares the first six with ===; the WebGL
# pair comes from OffscreenCanvas(1,1) in a Blob worker against a page canvas.
PROBES["worker"] = r"""async () => {
  const collect = () => {
    let vendor = 'NA', renderer = 'NA';
    try {
      const gl = (typeof document !== 'undefined' ? document.createElement('canvas') : new OffscreenCanvas(1, 1)).getContext('webgl');
      const e = gl && gl.getExtension('WEBGL_debug_renderer_info');
      if (e) { vendor = gl.getParameter(e.UNMASKED_VENDOR_WEBGL); renderer = gl.getParameter(e.UNMASKED_RENDERER_WEBGL); }
    } catch (err) {}
    const u = navigator.userAgentData;
    return {userAgent: navigator.userAgent, languages: JSON.stringify(navigator.languages), platform: navigator.platform,
      hardwareConcurrency: navigator.hardwareConcurrency, webGLVendor: vendor, webGLRenderer: renderer,
      deviceMemory: navigator.deviceMemory ?? null, userAgentData: u ? JSON.stringify([u.brands, u.mobile, u.platform]) : null,
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone};
  };
  const page = collect();
  const worker = await new Promise(res => {
    const w = new Worker(URL.createObjectURL(new Blob([`const collect = ${collect}; postMessage(collect());`], {type: 'text/javascript'})));
    w.onmessage = e => { res(e.data); w.terminate(); };
    w.onerror = e => res({error: String(e.message)});
    setTimeout(() => res({error: 'timeout'}), 8000);
  });
  const differ = Object.keys(page).filter(k => page[k] !== worker[k]).sort();
  return {differ, workerWebGLFailed: worker.webGLRenderer === 'NA' && page.webGLRenderer !== 'NA',
    page, worker: Object.fromEntries(differ.map(k => [k, worker[k]]))};
}"""

PROBES["keyboard"] = r"""async () => {
  if (!navigator.keyboard) return {error: 'no navigator.keyboard'};
  const m = await navigator.keyboard.getLayoutMap();
  const map = {}; for (const [k, v] of [...m.entries()].sort()) map[k] = v;
  return {size: m.size, map};
}"""

# Same offline graph and traps as the audio lane's smoke.py AUDIO_JS, plus the
# realtime context under every latencyHint.
PROBES["audio"] = r"""async () => {
  const oc = new OfflineAudioContext(1, 5000, 44100);
  const o = oc.createOscillator(); o.type = 'triangle'; o.frequency.value = 10000;
  const c = oc.createDynamicsCompressor(); c.threshold.value = -50; c.knee.value = 40; c.attack.value = 0;
  o.connect(c); c.connect(oc.destination); o.start(0);
  const b = await oc.startRendering();
  const data = b.getChannelData(0); const copy = new Float32Array(b.length); b.copyFromChannel(copy, 0);
  let sum = 0; for (const x of data) sum += Math.abs(x);
  const zc = new OfflineAudioContext(1, 100, 44100); const zo = zc.createOscillator(); zo.frequency.value = 0; zo.start(0);
  const silence = [...new Set((await zc.startRendering()).getChannelData(0))];
  const v = Math.fround(0.123456789);
  const ub = new AudioBuffer({length: 2000, sampleRate: 44100});
  for (const i of [300, 310, 320]) ub.getChannelData(0)[i] = v;
  const ucopy = new Float32Array(2000); ub.copyFromChannel(ucopy, 0);
  const written = [...ub.getChannelData(0)].every((x, i) => [300, 310, 320].includes(i) ? x === v : x === 0);
  const copied = [...ucopy].every((x, i) => x === ub.getChannelData(0)[i]);
  const ub2 = new AudioBuffer({length: 2000, sampleRate: 44100}); ub2.copyToChannel(new Float32Array(2000).fill(v), 0);
  const contexts = {};
  for (const [k, opts] of [['default', undefined], ['interactive', {latencyHint: 'interactive'}],
      ['balanced', {latencyHint: 'balanced'}], ['playback', {latencyHint: 'playback'}], ['sr44100', {sampleRate: 44100}],
      ['renderSizeHardware', {renderSizeHint: 'hardware'}]]) {
    try {
      const ac = new AudioContext(opts);
      await Promise.race([ac.resume(), new Promise(r => setTimeout(r, 1000))]);
      await new Promise(r => setTimeout(r, 1000));  // outputLatency settles after the first callbacks
      contexts[k] = {sampleRate: ac.sampleRate, baseLatency: ac.baseLatency, frames: Math.round(ac.baseLatency * ac.sampleRate),
        outputLatency: ac.outputLatency, maxChannelCount: ac.destination.maxChannelCount, state: ac.state,
        renderQuantumSize: ac.renderQuantumSize ?? null};
      await ac.close();
    } catch (e) { contexts[k] = {error: String(e)}; }
  }
  return {sum, readPathsAgree: data.every((x, i) => Object.is(x, copy[i])), silence,
    userBufferIntact: written && copied && ub2.getChannelData(0).every(x => x === v),
    sampleRate: contexts.default.sampleRate, baseLatency: contexts.default.baseLatency, contexts};
}"""

PROBES["heap"] = r"""async () => ({
  raw: performance.memory ? performance.memory.jsHeapSizeLimit : null,
  deviceMemory: navigator.deviceMemory ?? null, hardwareConcurrency: navigator.hardwareConcurrency,
})"""

PROBES["css"] = r"""async () => {
  const KW = ['AccentColor', 'AccentColorText', 'ActiveText', 'ButtonBorder', 'ButtonFace', 'ButtonText',
    'Canvas', 'CanvasText', 'Field', 'FieldText', 'GrayText', 'Highlight', 'HighlightText', 'LinkText',
    'Mark', 'MarkText', 'SelectedItem', 'SelectedItemText', 'VisitedText', 'ActiveBorder', 'ActiveCaption',
    'AppWorkspace', 'Background', 'ButtonHighlight', 'ButtonShadow', 'CaptionText', 'InactiveBorder',
    'InactiveCaption', 'InactiveCaptionText', 'InfoBackground', 'InfoText', 'Menu', 'MenuText', 'Scrollbar',
    'ThreeDDarkShadow', 'ThreeDFace', 'ThreeDHighlight', 'ThreeDLightShadow', 'ThreeDShadow', 'Window',
    'WindowFrame', 'WindowText', '-webkit-focus-ring-color'];
  const el = document.createElement('div'); document.body.appendChild(el);
  const colors = {};
  for (const k of KW) { el.style.backgroundColor = ''; el.style.backgroundColor = k; colors[k] = el.style.backgroundColor ? getComputedStyle(el).backgroundColor : null; }
  const systemFonts = {};
  for (const k of ['caption', 'icon', 'menu', 'message-box', 'small-caption', 'status-bar']) {
    el.style.font = ''; el.style.font = k; const cs = getComputedStyle(el);
    systemFonts[k] = [cs.fontFamily, cs.fontSize, cs.fontWeight];
  }
  el.remove();
  const mq = q => matchMedia(q).matches;
  const first = (f, vs) => vs.find(v => mq(`(${f}: ${v})`)) ?? null;
  const media = {
    colorGamut: first('color-gamut', ['rec2020', 'p3', 'srgb']),
    dynamicRange: first('dynamic-range', ['high', 'standard']),
    videoDynamicRange: first('video-dynamic-range', ['high', 'standard']),
    prefersColorScheme: first('prefers-color-scheme', ['dark', 'light']),
    prefersContrast: first('prefers-contrast', ['more', 'less', 'custom', 'no-preference']),
    forcedColors: first('forced-colors', ['active', 'none']),
    prefersReducedMotion: first('prefers-reduced-motion', ['reduce', 'no-preference']),
    prefersReducedTransparency: first('prefers-reduced-transparency', ['reduce', 'no-preference']),
    invertedColors: first('inverted-colors', ['inverted', 'none']),
    pointer: first('pointer', ['fine', 'coarse', 'none']), hover: first('hover', ['hover', 'none']),
    anyPointer: first('any-pointer', ['fine', 'coarse', 'none']), anyHover: first('any-hover', ['hover', 'none']),
    color: [16, 12, 10, 8, 6, 4, 1].find(n => mq(`(min-color: ${n})`)) ?? 0,
    monochrome: mq('(monochrome)'),
  };
  const s = screen;
  const scr = {width: s.width, height: s.height, availWidth: s.availWidth, availHeight: s.availHeight,
    availTop: s.availTop, availLeft: s.availLeft, colorDepth: s.colorDepth, pixelDepth: s.pixelDepth,
    isExtended: s.isExtended, orientation: s.orientation && s.orientation.type, dpr: devicePixelRatio,
    outerWidth, outerHeight, innerWidth, innerHeight, screenX, screenY};
  const TEXT = 'The quick brown fox jumps over the lazy dog 0123456789';
  const ctx = document.createElement('canvas').getContext('2d');
  const span = document.createElement('span'); span.textContent = TEXT; span.style.cssText = 'position:absolute;white-space:nowrap;font-size:16px';
  document.body.appendChild(span);
  const fontWidths = {};
  for (const f of ['system-ui', '-apple-system', 'BlinkMacSystemFont', 'Helvetica', 'Helvetica Neue', 'Segoe UI', 'Arial', 'Verdana', 'sans-serif']) {
    const fam = f.includes(' ') ? `"${f}"` : f;
    ctx.font = `16px ${fam}`; span.style.fontFamily = fam;
    fontWidths[f] = {measureText: ctx.measureText(TEXT).width, span: span.getBoundingClientRect().width};
  }
  span.remove();
  return {colors, systemFonts, media, screen: scr, fontWidths};
}"""

PROBES["referrer"] = r"""async () => {
  const P = window.__probe, A = location.origin, B = P.b;
  const norm = s => s == null ? s : s.split(A).join('A').split(B).join('B');
  const echo = async u => (await fetch(u, {cache: 'no-store'})).json();
  const frame = await new Promise(res => {
    const f = document.createElement('iframe');
    const h = e => { if (e.data && 'ref' in e.data) { removeEventListener('message', h); f.remove(); res(e.data); } };
    addEventListener('message', h); f.src = B + '/frame'; document.body.appendChild(f);
    setTimeout(() => res(null), 5000);
  });
  return {page: norm(location.href), policy: document.querySelector('meta[name=referrer]') ? 'meta' : 'default',
    fetchSameOrigin: norm((await echo(A + '/echo')).referer ?? null),
    fetchCrossOrigin: norm((await echo(B + '/echo')).referer ?? null),
    iframeCrossOriginHeader: frame && norm(frame.header), iframeCrossOriginDocument: frame && norm(frame.ref)};
}"""

PROBES["uach_headers"] = r"""async () => {
  const P = window.__probe;
  const pick = h => Object.fromEntries(Object.entries(h || {}).filter(([k]) => k.startsWith('sec-ch-') || k === 'user-agent').sort());
  const echo = async u => (await fetch(u, {cache: 'no-store'})).json();
  const ua = navigator.userAgentData;
  return {navigation: pick(P.nav), fetchSameOrigin: pick(await echo(location.origin + '/echo')),
    fetchCrossOrigin: pick(await echo(P.b + '/echo')),
    highEntropy: ua ? await ua.getHighEntropyValues(['architecture', 'bitness', 'model', 'platformVersion', 'fullVersionList', 'wow64', 'formFactors']) : null};
}"""

PROBES["prefs"] = r"""async () => {
  const out = {paymentRequest: typeof PaymentRequest};
  const timeout = p => Promise.race([p, new Promise(r => setTimeout(() => r('timeout'), 4000))]);
  const total = {total: {label: 't', amount: {currency: 'USD', value: '1.00'}}};
  const methods = {googlePay: {supportedMethods: 'https://google.com/pay'},
    basicCard: {supportedMethods: 'basic-card'}};
  for (const [k, m] of Object.entries(methods)) {
    try { out['canMakePayment.' + k] = await timeout(new PaymentRequest([m], total).canMakePayment()); } catch (e) { out['canMakePayment.' + k] = 'error:' + e.name; }
    try { out['hasEnrolledInstrument.' + k] = await timeout(new PaymentRequest([m], total).hasEnrolledInstrument()); } catch (e) { out['hasEnrolledInstrument.' + k] = 'error:' + e.name; }
  }
  Object.assign(out, {doNotTrack: navigator.doNotTrack, globalPrivacyControl: navigator.globalPrivacyControl ?? null,
    pdfViewerEnabled: navigator.pdfViewerEnabled, cookieEnabled: navigator.cookieEnabled,
    passwordCredential: typeof PasswordCredential, federatedCredential: typeof FederatedCredential,
    chromeHeight: outerHeight - innerHeight, chromeWidth: outerWidth - innerWidth,
    cpuPerformance: navigator.cpuPerformance ?? null});
  // Hyperlink auditing: ungoogled builds drop <a ping>, and the server sees it.
  const f = document.createElement('iframe'); f.name = 'pingframe'; document.body.appendChild(f);
  const a = document.createElement('a'); a.href = '/echo?nav'; a.ping = '/ping'; a.target = 'pingframe';
  document.body.appendChild(a); a.click();
  await new Promise(r => setTimeout(r, 1500));
  out.aPingSent = (await (await fetch('/udp', {cache: 'no-store'})).json()).pings > 0;
  a.remove(); f.remove();
  return out;
}"""

PROBES["reportingobserver"] = r"""async () => {
  if (typeof ReportingObserver === 'undefined') return {present: false};
  const got = [];
  const ro = new ReportingObserver(rs => rs.forEach(r => got.push(r.type + ':' + (r.body && r.body.id))), {buffered: true});
  ro.observe();
  const x = new XMLHttpRequest(); x.open('GET', '/echo', false); x.send();  // a deprecation report on real Chrome
  await new Promise(r => setTimeout(r, 1500));
  ro.takeRecords().forEach(r => got.push(r.type + ':' + (r.body && r.body.id)));
  ro.disconnect();
  return {present: true, reports: [...new Set(got)].sort()};
}"""


# Evaluated on deviceandbrowserinfo.com/are_you_a_bot once it has rendered: the
# page's own main-thread and worker values behind hasInconsistentWorkerValues.
DABI_WORKER_JS = (
    "(() => { const f = window.fingerprint, w = f && f.workerData; if (!w) return null;"
    " return {ua: [f.userAgent, w.userAgent], plat: [navigator.platform, w.platform],"
    " hc: [navigator.hardwareConcurrency, w.hardwareConcurrency],"
    " langs: [JSON.stringify(navigator.languages), JSON.stringify(w.languages)],"
    " v: [f.webGLVendor, w.webGLVendor], r: [f.webGLRenderer, w.webGLRenderer]}; })()")

_KEEP_HEADERS = ("user-agent", "referer", "origin", "accept-language")


class _Server:
    """Origins A and B on 127.0.0.1, a STUN packet counter and an <a ping> counter."""

    def __enter__(self) -> "_Server":
        self.udp_packets = 0
        self.pings = 0
        self.udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.udp.bind(("0.0.0.0", 0))
        self.udp.settimeout(0.5)
        self.udp_host = _primary_ip()
        self._stop = False
        srv = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a) -> None:
                pass

            def _send(self, body: str, ctype: str) -> None:
                data = body.encode()
                self.send_response(200)
                self.send_header("Content-Type", ctype)
                self.send_header("Access-Control-Allow-Origin", "*")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_POST(self) -> None:
                if self.path.split("?")[0] == "/ping":
                    srv.pings += 1
                self._send("", "text/plain")

            def do_GET(self) -> None:
                hdrs = {k.lower(): v for k, v in self.headers.items()
                        if k.lower().startswith("sec-") or k.lower() in _KEEP_HEADERS}
                path = self.path.split("?")[0]
                if path == "/echo":
                    self._send(json.dumps(hdrs), "application/json")
                elif path == "/udp":
                    self._send(json.dumps({"packets": srv.udp_packets, "pings": srv.pings}), "application/json")
                elif path == "/frame":
                    ref = json.dumps(hdrs.get("referer"))
                    self._send("<!doctype html><script>parent.postMessage({ref: document.referrer,"
                               f" header: {ref}}}, '*')</script>", "text/html")
                else:
                    cfg = json.dumps({"b": srv.origin_b, "nav": hdrs,
                                      "udpHost": srv.udp_host, "udpPort": srv.udp.getsockname()[1]})
                    self._send(f"<!doctype html><title>probe</title><body><script>window.__probe = {cfg};"
                               "</script></body>", "text/html")

        self.http = [ThreadingHTTPServer(("127.0.0.1", 0), H) for _ in range(2)]
        self.origin_a, self.origin_b = (f"http://127.0.0.1:{s.server_address[1]}" for s in self.http)
        self.threads = [threading.Thread(target=s.serve_forever, daemon=True) for s in self.http]
        self.threads.append(threading.Thread(target=self._count_udp, daemon=True))
        for t in self.threads:
            t.start()
        return self

    def _count_udp(self) -> None:
        while not self._stop:
            try:
                self.udp.recvfrom(2048)
                self.udp_packets += 1
            except OSError:
                pass

    def __exit__(self, *exc) -> None:
        self._stop = True
        for s in self.http:
            s.shutdown()
            s.server_close()
        self.udp.close()


def _primary_ip() -> str:
    """The address a WebRTC host candidate would use; connect() on UDP sends nothing."""
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        s.connect(("192.0.2.1", 9))
        return s.getsockname()[0]
    except OSError:
        return "127.0.0.1"
    finally:
        s.close()


def _eval(conn, name: str, src: str):
    expr = (f"(async () => {{ const t = new Promise((_, j) => setTimeout(() => j(new Error('timeout')), {PROBE_TIMEOUT_MS}));"
            f" try {{ return JSON.stringify(await Promise.race([({src})(), t])); }}"
            " catch (e) { return JSON.stringify({error: String(e)}); } })()")
    try:
        # userGesture: an AudioContext only starts, and reports outputLatency,
        # after activation - as it would once a person has clicked the page.
        r = conn.cmd("Runtime.evaluate", {"expression": expr, "returnByValue": True,
                                          "awaitPromise": True, "userGesture": True})
        raw = r.get("result", {}).get("result", {}).get("value")
    except Exception as e:
        return {"error": f"{type(e).__name__}: {e}"}
    if not isinstance(raw, str):
        return {"error": f"no value from {name}"}
    return json.loads(raw)


def run(conn) -> dict:
    """Every probe, then the cdp probe again driver-shaped (Runtime enabled)."""
    out: dict = {}
    with _Server() as srv:
        conn.cmd("Page.navigate", {"url": srv.origin_a + "/probe/page?q=1"})
        for _ in range(40):
            try:
                if conn.eval("document.readyState === 'complete' && !!window.__probe", True) is True:
                    break
            except Exception:
                pass
            time.sleep(0.25)
        for name, src in PROBES.items():
            out[name] = _eval(conn, name, src)
        if "cdp" in PROBES:
            conn.cmd("Runtime.enable", {})
            try:
                out["cdp_runtime_enabled"] = _eval(conn, "cdp", PROBES["cdp"])
            finally:
                conn.cmd("Runtime.disable", {})
    return out
