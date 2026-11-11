# Sentinel Design System Guidelines (`impeccable`)

> **Register:** Product (Developer Tool / Observability & Incident Response Dashboard)  
> **Physical Scene:** SRE / On-call engineer glancing at issue severity and stack traces on a 27-inch monitor at 2am in a dim room or an incident command center.  
> **Color Strategy:** Committed Product Palette — Tinted OKLCH neutrals with semantic role allocations (critical, warning, resolved, primary).

---

## 1. Principles & Laws

1. **Information Density with Breathing Room**:
   - Technical observability requires high scannability. Hierarchy is established through typography scale and weight contrast (≥1.25 ratio), never nested cards.
2. **Absolute Bans**:
   - **No side-stripe borders**: Avoid colored `border-left` / `border-right` accents on cards, lists, or alerts. Use full borders with subtle background tints and semantic icon pills.
   - **No gradient text**: Avoid decorative `background-clip: text` gradients. Solid tokens ensure maximum clarity under pressure.
   - **No nested cards**: Containers should not be wrapped inside unnecessary wrapper boxes.
3. **Motion**:
   - Transitions use exponential ease-out curves (`cubic-bezier(0.16, 1, 0.3, 1)`). Layout properties (`height`, `width`, `margin`) are not animated directly.
4. **Color & Contrast**:
   - OKLCH color space used across all palettes.
   - Pure `#000000` and `#ffffff` are banned. Neutrals are tinted toward deep slate/blue (`oklch(0.18 0.025 255)`).
   - High contrast ratios (WCAG AAA for text, AA for UI controls).

---

## 2. OKLCH Semantic Design Tokens

Tokens defined in `src/app.css`:

```css
:root {
  /* OKLCH Base Neutrals */
  --bg-root: oklch(0.16 0.024 260);          /* Deep slate backdrop */
  --bg-surface: oklch(0.20 0.025 260);       /* Elevation 1: panels & headers */
  --bg-surface-elevated: oklch(0.24 0.028 260); /* Elevation 2: dropdowns & modals */
  --bg-surface-hover: oklch(0.26 0.028 260);

  --border-subtle: oklch(0.28 0.022 260);
  --border-muted: oklch(0.34 0.024 260);
  --border-focus: oklch(0.65 0.18 250);

  /* Typography Colors */
  --text-primary: oklch(0.96 0.008 260);
  --text-secondary: oklch(0.78 0.015 260);
  --text-muted: oklch(0.62 0.018 260);
  --text-subtle: oklch(0.48 0.018 260);

  /* Interactive / Brand */
  --color-primary: oklch(0.62 0.19 255);
  --color-primary-hover: oklch(0.55 0.20 255);
  --color-primary-subtle: oklch(0.62 0.19 255 / 0.15);

  /* Semantic Status & Severities */
  --severity-critical-bg: oklch(0.35 0.12 25 / 0.25);
  --severity-critical-text: oklch(0.72 0.19 25);
  --severity-critical-border: oklch(0.45 0.14 25);

  --severity-warning-bg: oklch(0.40 0.10 75 / 0.25);
  --severity-warning-text: oklch(0.78 0.16 75);
  --severity-warning-border: oklch(0.50 0.12 75);

  --status-resolved-bg: oklch(0.35 0.10 150 / 0.25);
  --status-resolved-text: oklch(0.75 0.15 150);
  --status-resolved-border: oklch(0.45 0.11 150);

  --status-ignored-bg: oklch(0.30 0.02 260 / 0.3);
  --status-ignored-text: oklch(0.70 0.02 260);
  --status-ignored-border: oklch(0.38 0.02 260);
}
```
