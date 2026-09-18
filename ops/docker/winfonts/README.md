# winfonts

The Windows persona's font pack (`/opt/personafonts` in the amd64 image) is
**generated at build time** by the `personafonts-amd64` stage in
`ops/docker/Dockerfile` - it is no longer committed as binaries. The arm64 image
carries the macOS counterpart, built the same way by `personafonts-arm64`; the
licenses of both are in `docs/THIRD-PARTY.md`. The stage
installs free, metric- or coverage-compatible fonts from Debian main and rewrites
each font's internal `name` table (via `scripts/rename-fonts.py`) to report the
Windows family it stands in for. A Windows-claiming fingerprint is expected to
expose these family names, and anti-bot JS both enumerates fonts (measuring text
width) and rasterises glyphs (emoji/CJK canvas-hash checks), so the substitutes
must render real glyphs AND report the Windows family name.

**No Microsoft font software is included.** These are free, redistributable fonts
with only their `name` table rewritten:

| Windows family reported | Free font used         | Debian package             | License |
| ----------------------- | ---------------------- | -------------------------- | ------- |
| Arial                   | Liberation Sans        | fonts-liberation2          | OFL 1.1 |
| Times New Roman         | Liberation Serif       | fonts-liberation2          | OFL 1.1 |
| Courier New             | Liberation Mono        | fonts-liberation2          | OFL 1.1 |
| Calibri, Segoe UI       | Carlito                | fonts-crosextra-carlito    | OFL 1.1 |
| Cambria                 | Caladea                | fonts-crosextra-caladea    | OFL 1.1 |
| Segoe UI Emoji          | Noto Color Emoji       | fonts-noto-color-emoji     | OFL 1.1 |
| Microsoft YaHei         | WenQuanYi Zen Hei      | fonts-wqy-zenhei           | GPL-2 w/ font embedding exception, M+ |
| Yu Gothic               | IPAPGothic             | fonts-ipafont-gothic       | IPA 1.0 |
| MS Gothic               | IPAGothic              | fonts-ipafont-gothic       | IPA 1.0 |
| Leelawadee UI           | Loma                   | fonts-tlwg-loma-otf        | GPL-2+ w/ font exception |

Plus `cuttle-null` (built by `scripts/make-null-font.py`): a glyphless sink font.

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
