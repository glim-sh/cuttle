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
redistributable fonts with only their `name` table rewritten (and, where the
last column says so, the real font's metrics stamped on - see below):

| Windows family reported | Free font used         | Debian package             | License | Metrics stamped |
| ----------------------- | ---------------------- | -------------------------- | ------- | --------------- |
| Arial                   | Liberation Sans        | fonts-liberation2          | OFL 1.1 | no (metric-compatible) |
| Times New Roman         | Liberation Serif       | fonts-liberation2          | OFL 1.1 | no (metric-compatible) |
| Courier New             | Liberation Mono        | fonts-liberation2          | OFL 1.1 | no (metric-compatible) |
| Segoe UI Emoji          | Noto Color Emoji       | fonts-noto-color-emoji     | OFL 1.1 | no |
| Leelawadee UI           | Loma                   | fonts-tlwg-loma-otf        | GPL-2+ w/ font exception | no |
| Segoe UI (Regular, Bold, Light) | Selawik 1.01   | GitHub release, sha256-pinned | OFL 1.1 | yes |
| Segoe UI (Italic, Bold Italic, Light Italic) | Carlito | fonts-crosextra-carlito | OFL 1.1 | yes |
| Calibri, Candara (4 faces each) | Carlito        | fonts-crosextra-carlito    | OFL 1.1 | yes |
| Tahoma (Regular, Bold), Trebuchet MS (4 faces) | Carlito | fonts-crosextra-carlito | OFL 1.1 | yes |
| Cambria, Constantia, Sitka Small (4 faces each) | Caladea | fonts-crosextra-caladea | OFL 1.1 | yes |
| Georgia (4 faces), Cambria Math, Gabriola, MingLiU-ExtB, MingLiU_HKSCS-ExtB | Caladea | fonts-crosextra-caladea | OFL 1.1 | yes |
| Corbel (4 faces), Franklin Gothic (Medium, Medium Italic), Verdana Italic and Bold Italic | Liberation Sans | fonts-liberation2 | OFL 1.1 | yes |
| Palatino Linotype (4 faces), Microsoft Himalaya, Microsoft Yi Baiti, Mongolian Baiti, SimSun, SimSun-ExtB, PMingLiU-ExtB, Symbol | Liberation Serif | fonts-liberation2 | OFL 1.1 | yes |
| Ebrima, Gadugi, Microsoft JhengHei, Microsoft JhengHei UI, Microsoft New Tai Lue, Microsoft PhagsPa, Microsoft Tai Le, Myanmar Text, Nirmala UI (Regular, Bold each); Javanese Text; Microsoft YaHei Bold, Yu Gothic Bold | Selawik 1.01 | GitHub release, sha256-pinned | OFL 1.1 | yes |
| Verdana (Regular, Bold), Impact, Segoe UI Symbol, Lucida Sans Unicode, Microsoft Sans Serif, Bahnschrift (Regular, Bold), MS PGothic, MS UI Gothic, Webdings, Marlett, Segoe MDL2 Assets, Segoe Fluent Icons | DejaVu Sans | fonts-dejavu-core | Bitstream Vera | yes |
| Consolas (4 faces), Lucida Console | DejaVu Sans Mono       | fonts-dejavu-core          | Bitstream Vera | yes |
| Sylfaen                 | DejaVu Serif           | fonts-dejavu-core          | Bitstream Vera | yes |
| Comic Sans MS (4 faces), Segoe Print (Regular, Bold), Segoe Script (Regular, Bold), Ink Free, MV Boli | Comic Neue | fonts-comic-neue | OFL 1.1 | yes |
| Microsoft YaHei, Malgun Gothic (Regular, Bold) | WenQuanYi Zen Hei | fonts-wqy-zenhei           | GPL-2 w/ font embedding exception, M+ | yes (Latin) |
| Yu Gothic               | IPAPGothic             | fonts-ipafont-gothic       | IPA 1.0 | yes (Latin) |
| MS Gothic               | IPAGothic              | fonts-ipafont-gothic       | IPA 1.0 | yes (Latin) |

Plus `cuttle-null` (built by `scripts/make-null-font.py`): a glyphless sink font.

That covers all 55 families of a stock Windows 10/11 core font set (the list
Pixelscan checks) and the stock faces detectors probe beyond it (Segoe UI Light,
Nirmala UI, Ink Free, Segoe MDL2 Assets, Segoe Fluent Icons), with every face
real Windows ships for them. A stamped face takes the real face's advances,
vertical metrics (Windows lays a line out from the win metrics, or the typo
ones under USE_TYPO_METRICS, with no line gap), kerning, style, weight, and
full and PostScript names, so which free font a family is built from decides
only its glyph shapes, and several families share one. It keeps only the
codepoints the real face covers (a codepoint the real face lacks falls back, as
on Windows; the icon fonts keep just a space) and forms none of its own
ligatures; the real face's two-letter ligatures (`fi`, `fl`) arrive as kerning
pairs. The CJK families (YaHei, Malgun Gothic, Yu Gothic, MS Gothic, SimSun,
MingLiU and the like) are stamped over Latin only; the four whose source has
CJK glyphs keep the CJK and Hangul the real face covers, the rest render it from
those four. Arabic comes only from Microsoft Sans Serif, whose source (DejaVu
Sans) draws it and whose real Arabic advances match Times New Roman's, the face
Windows Chrome falls back to for Arabic. The build fails if no face covers
`:lang=ko` or `:lang=ar`.

Known gaps, all measured against real Chrome 156 on Windows 11 (72px):

- Windows widens a synthesized bold by about 1/50 em per glyph for some
  single-weight fonts (Cambria Math, Lucida Console, MS Gothic, MS PGothic, MS
  UI Gothic, SimSun, SimSun-ExtB); the Linux build does not, so their bold reads
  up to 3.4% narrow.
- Segoe Script's contextual alternates (`calt`) are not reproduced: 0.6% wide.
- FreeType hints any family whose name contains "MingLiU" natively and rounds
  its advances to whole pixels, so PMingLiU-ExtB reads 0.1% off.
- Segoe UI Emoji is Noto Color Emoji, which has no Latin glyphs; the real
  font's Latin is Segoe UI's.
- Leelawadee UI is unstamped Loma.
- The math generic (Cambria Math) has real widths but no MATH table, as on
  the macOS persona.

"Segoe UI" is also what CSS `system-ui` resolves to on the Windows persona
(patch 0063), so its four faces carry Segoe UI's own advance widths, vertical
metrics and kerning from `metrics.json`, as do the other stamped families above
(their kerning over U+0020-U+017F). The table is extracted from a real Windows 11
install with `scripts/extract-font-metrics.py windows --src <copy of the fonts
it lists from C:\Windows\Fonts> --targets <dir with selawk*.ttf and
Carlito-*.ttf>` (integers only; the Microsoft fonts are never committed). The
kerning is Segoe UI's pairs as HarfBuzz applies them (GPOS `kern` for Regular and Bold, the legacy `kern` table for the italics),
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
