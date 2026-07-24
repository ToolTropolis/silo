# Silo brand assets

| File | Use |
|---|---|
| `logo.svg` | The mark. Primary asset — scales to any size. |
| `logo-wordmark.svg` | Mark + "Silo" wordmark, for headers and READMEs. |
| `logo-32.png` · `logo-128.png` · `logo-512.png` | Raster fallbacks where SVG isn't accepted. |
| `logo-512-onDark.png` | White mark, for dark backgrounds. |

## The concept

A silo silhouette — domed cap, tall narrow body — holding three stacked bands.
The bands are versioned memory layers, fading as they recede, and they sit
inset from the wall so the enclosing boundary reads as deliberate. That
boundary is the product: per-project isolation that nothing crosses.

## Theming

Both SVGs use `currentColor` for every stroke and fill, so the mark inherits the
surrounding text color and works on light and dark backgrounds with no separate
variant:

```html
<span style="color: #1a1a19"><!-- logo.svg inlined --></span>
```

For contexts that can't inherit color (favicons, app icons, third-party
uploads), use the PNGs — `logo-512-onDark.png` for dark backgrounds.

## Favicon

`logo.svg` stays legible down to 16px: the silhouette holds and the interior
bands merge into texture rather than mud. Verified at 16/32/48px.

```html
<link rel="icon" href="/docs/brand/logo.svg" type="image/svg+xml">
<link rel="alternate icon" href="/docs/brand/logo-32.png">
```

## Constraints

- Don't recolor parts of the mark independently — it's designed as one color
  plus opacity steps.
- Don't add effects (gradients, shadows, outlines). The mark carries at small
  sizes because it's flat and high-contrast.
- Keep clear space around the mark equal to the width of the domed cap.

## Status

Hand-authored geometric mark, not a professional identity. It's honest,
legible, and consistent — good enough to ship. If Silo ever needs a real brand,
this is a starting point to hand a designer, not a finished system.

## favicon.ico

Multi-resolution icon (16/32/48/64/128/256) built from `logo-512.png`, used by
the dashboard and as the repo icon.

`web/dashboard/static/` holds a copy because `go:embed` cannot reference paths
outside its own package directory. Regenerate both together if the mark changes.
