# Architecture diagrams

`system-arch.drawio` is the editable source; `system-arch.svg` is the rendered
copy embedded in [`../architecture.md`](../architecture.md).

## Re-exporting the SVG

**Use these exact flags.** The draw.io default embeds fonts *and* a rasterized
PNG copy of every text label as base64, which produced a **1.2 MB** SVG where
96% of the bytes were pictures of text. That bloats the repo and makes diffs
unreviewable — one label edit rewrites the whole base64 blob.

```bash
drawio --export --format svg --embed-svg-fonts false \
  --output docs/architecture/system-arch.svg \
  docs/architecture/system-arch.drawio --no-sandbox
```

That yields ~56 KB with real `<text>` elements, so non-browser renderers
(rsvg-convert, thumbnailers, CI screenshot tools) show actual labels.

Export is **asynchronous** — poll for the output file to exist and be non-empty
rather than trusting the exit code.

Afterwards, strip draw.io's trailing "Text is not SVG - cannot display" notice.
It's a global fallback block that is misleading once real `<text>` is present:

```bash
python3 - <<'PY'
import re, pathlib
p = pathlib.Path('docs/architecture/system-arch.svg')
s = p.read_text()
s = re.sub(
    r'<switch>\s*<g requiredFeatures="[^"]*"/>\s*<a[^>]*svg-export-text-problems[^>]*>.*?</a>\s*</switch>',
    '', s, flags=re.S)
p.write_text(s)
PY
```

## Editing

Open the `.drawio` at [app.diagrams.net](https://app.diagrams.net) or with the
VS Code Draw.io Integration extension. Re-export with the command above, then
re-run the diagram QA pass before committing.
