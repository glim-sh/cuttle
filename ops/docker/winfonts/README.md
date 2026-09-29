# winfonts

The Windows persona's font pack (`/opt/personafonts` in the amd64 image) is
**generated at build time** by the `personafonts-amd64` stage in
`ops/docker/Dockerfile` - it is no longer committed as binaries. The arm64 image
carries the macOS counterpart, built the same way by `personafonts-arm64`; the
licenses of both are in `docs/THIRD-PARTY.md`. The stage
installs free, metric- or coverage-compatible fonts from Debian main (plus a
pinned Selawik release from Microsoft's GitHub) and rewrites
each font's internal `name` table (via `scripts/rename-fonts.py`) to report the
Windows family it stands in for. A Windows-claiming fingerprint is expected to
expose these family names, and anti-bot JS both enumerates fonts (measuring text
width) and rasterises glyphs (emoji/CJK canvas-hash checks), so the substitutes
must render real glyphs AND report the Windows family name.

**No proprietary Microsoft font software is included.** These are free,
redistributable fonts with only their `name` table rewritten (and, for Segoe UI,
its advance widths and kerning stamped on - see below):

| Windows family reported | Free font used         | Debian package             | License |
| ----------------------- | ---------------------- | -------------------------- | ------- |
| Arial                   | Liberation Sans        | fonts-liberation2          | OFL 1.1 |
| Times New Roman         | Liberation Serif       | fonts-liberation2          | OFL 1.1 |
| Courier New             | Liberation Mono        | fonts-liberation2          | OFL 1.1 |
| Calibri                 | Carlito                | fonts-crosextra-carlito    | OFL 1.1 |
| Segoe UI (Regular, Bold) | Selawik 1.01          | GitHub release, sha256-pinned | OFL 1.1 |
| Segoe UI (Italic, Bold Italic) | Carlito         | fonts-crosextra-carlito    | OFL 1.1 |
| Cambria                 | Caladea                | fonts-crosextra-caladea    | OFL 1.1 |
| Segoe UI Emoji          | Noto Color Emoji       | fonts-noto-color-emoji     | OFL 1.1 |
| Microsoft YaHei         | WenQuanYi Zen Hei      | fonts-wqy-zenhei           | GPL-2 w/ font embedding exception, M+ |
| Yu Gothic               | IPAPGothic             | fonts-ipafont-gothic       | IPA 1.0 |
| MS Gothic               | IPAGothic              | fonts-ipafont-gothic       | IPA 1.0 |
| Leelawadee UI           | Loma                   | fonts-tlwg-loma-otf        | GPL-2+ w/ font exception |

Plus `cuttle-null` (built by `scripts/make-null-font.py`): a glyphless sink font.

"Segoe UI" is also what CSS `system-ui` resolves to on the Windows persona
(patch 0063), so its four faces carry Segoe UI's own advance widths, vertical
metrics and kerning from `metrics.json`, extracted from a real Windows 11 install
with `scripts/extract-font-metrics.py windows --src <copy of C:\Windows\Fonts>
--targets <dir with selawk*.ttf and Carlito-*.ttf>` (integers only; the Microsoft
fonts are never committed). The kerning is Segoe UI's pairs as HarfBuzz applies
them (GPOS `kern` for Regular and Bold, the legacy `kern` table for the italics),
flattened to codepoint pairs over the codepoints each stand-in has, and replaces
the stand-in's own kerning as one GPOS `kern` lookup. Selawik, Microsoft's
metric-compatible Segoe UI stand-in, already has Segoe UI's Latin advances, so
the stamp mainly fixes its vertical metrics and adds the kerning it lacks. It
covers Latin only (348 codepoints against Segoe UI's ~4000).

## Why enumeration lockdown is needed on top of renaming

Renaming makes the fonts *report* Windows names, but two Linux tells remain that
the Dockerfile closes in the runtime stage:

1. **fontconfig aliases.** `/etc/fonts/conf.d/30-metric-aliases.conf` aliases
   Arial<->Liberation Sans, Calibri<->Carlito, Cambria<->Caladea, so a request
   for the Linux name resolves to our renamed font. The stage deletes that conf
   and restricts `fonts.conf` to `/opt/personafonts` only.
2. **Chromium's hardcoded equivalence table.** `SkFontConfigInterface_direct.cpp`
   groups metric-compatible families (SANS = Arial/Arimo/Liberation Sans, etc.)
   and accepts a substitute when the *requested* family and the *matched font's*
   family share a class - comparing the original request, which no fontconfig
   alias can intercept. `50-block-linux-aliases.conf` rewrites those Linux names
   to the glyphless `cuttle-null` sink font, so the browser finds no glyphs and
   falls through to the CSS generic - matching a real Windows Chrome, where the
   Linux family simply does not exist.

To regenerate locally, run the `personafonts-amd64`-stage commands from the
Dockerfile, or `docker build --platform linux/amd64 --target personafonts-amd64
-f ops/docker/Dockerfile .`.
