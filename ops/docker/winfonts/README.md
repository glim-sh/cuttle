# winfonts

Metric-compatible free fonts whose internal `name` table has been set to report
the corresponding Windows family. A Windows-claiming fingerprint is expected to
expose these family names, and some anti-bot JS enumerates fonts by measuring
text width, so the substitutes must render at Windows metrics and report the
Windows family name.

**No Microsoft font software is included here.** These are free, redistributable
fonts with only their family name rewritten:

| Windows family reported | Free font used            | License              |
| ----------------------- | ------------------------- | -------------------- |
| Arial                   | Liberation Sans           | OFL 1.1              |
| Times New Roman         | Liberation Serif          | OFL 1.1              |
| Courier New             | Liberation Mono           | OFL 1.1              |
| Calibri, Segoe UI       | Carlito                   | OFL 1.1              |
| Cambria                 | Caladea                   | OFL 1.1              |

The Dockerfile copies these into `/opt/winfonts` and registers them with
fontconfig; `cuttle serve` passes `--fingerprint-fonts-dir=/opt/winfonts` to the
fork binaries. To regenerate from upstream Debian packages, install
`fonts-liberation2 fonts-crosextra-carlito fonts-crosextra-caladea` and rewrite
the `name` table records (family/full/postscript/typographic) to the target
family.
