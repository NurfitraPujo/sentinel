---
name: Sentinel
description: High-density error tracking & observability system
colors:
  primary: "#3b82f6"
  primary-deep: "#1d4ed8"
  neutral-bg: "#0f172a"
  surface: "#1e293b"
  surface-border: "#334155"
  text-main: "#f8fafc"
  text-muted: "#94a3b8"
  severity-critical: "#ef4444"
  severity-warning: "#f59e0b"
  severity-info: "#3b82f6"
  status-resolved: "#10b981"
  status-ignored: "#94a3b8"
typography:
  display:
    fontFamily: "Inter, system-ui, -apple-system, sans-serif"
    fontSize: "1.75rem"
    fontWeight: 600
    lineHeight: 1.2
  headline:
    fontFamily: "Inter, system-ui, -apple-system, sans-serif"
    fontSize: "1.25rem"
    fontWeight: 600
    lineHeight: 1.3
  body:
    fontFamily: "Inter, system-ui, -apple-system, sans-serif"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: 1.5
  label:
    fontFamily: "JetBrains Mono, monospace"
    fontSize: "0.75rem"
    fontWeight: 500
    letterSpacing: "0.05em"
rounded:
  sm: "4px"
  md: "6px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "16px"
  lg: "24px"
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.text-main}"
    rounded: "{rounded.sm}"
    padding: "6px 12px"
  card-surface:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.text-main}"
    rounded: "{rounded.md}"
    padding: "16px"
---

# Design System: Sentinel

## 1. Overview

**Creative North Star: "The Incident Control Room"**

Sentinel is built for rapid error triage, log inspection, support ticket tracking, and real-time observability. Its visual system prioritizes extreme data density, instant scannability, and high-contrast status feedback. The interface assumes a dark environment, reducing eye strain during high-pressure incidents while emphasizing severity indicators with surgical precision.

### Key Characteristics:
- **High Information Density**: Compact tables, zero-padding bloat, and sharp tabular alignment.
- **Precision Color System**: Monochromatic slate-gray surfaces with high-chroma status accents reserved exclusively for impact/severity.
- **Monospace Telemetry**: Monospace typography for stack traces, error IDs, timestamps, and key-value attributes.
- **Zero Decorative Fluff**: Rejects glassmorphism, decorative text gradients, and ambient blurs.

## 2. Colors

A disciplined dark palette constructed with cool slate neutrals and functional signal accents.

### Primary
- **Signal Blue** (`#3b82f6`): Primary action buttons, active tab indicators, and info-level events.

### Neutral
- **Slate Void** (`#0f172a`): Root application background.
- **Control Surface** (`#1e293b`): Container panels, table headers, sidebars, and drawer surfaces.
- **Surface Border** (`#334155`): Crisp 1px structural borders dividing views and table rows.
- **Text Main** (`#f8fafc`): High contrast primary text and error messages.
- **Text Muted** (`#94a3b8`): Secondary metadata, timestamps, and column labels.

### Status & Severity
- **Critical Red** (`#ef4444`): High severity exceptions, active alerts, and error spikes.
- **Warning Amber** (`#f59e0b`): Degradation warnings, rate limits, and unhandled warnings.
- **Resolved Emerald** (`#10b981`): Resolved issue state indicators.
- **Ignored Slate** (`#94a3b8`): Muted/ignored issue state indicators.

### Named Rules
**The Signal-to-Noise Rule.** Saturated color carries less than 5% of any screen surface. Color is used strictly for severity classification and focused interactives; it never serves as background decoration.

## 3. Typography

**Display & Body Font:** Inter / System UI Font Stack  
**Label & Code Font:** JetBrains Mono / System Monospace Stack  

### Hierarchy
- **Display** (600, 1.75rem, 1.2): Primary view titles and metric headers.
- **Headline** (600, 1.25rem, 1.3): Section headers and issue title summaries.
- **Body** (400, 0.875rem, 1.5): Main table text, descriptions, and user notes.
- **Label / Code** (500, 0.75rem, 0.05em): Error codes, stack trace lines, status badges, timestamps.

### Named Rules
**The Tabular Precision Rule.** All numeric metrics, error counts, timestamps, and hash IDs must use tabular numbers (`font-variant-numeric: tabular-nums`) or monospace fonts for vertical visual alignment.

## 4. Elevation

Sentinel operates on flat, structural layer separation rather than realistic physical shadows.

- **Surface Layering**: Depth is created strictly through background contrast (Slate Void `#0f172a` vs Control Surface `#1e293b`) and crisp 1px borders (`#334155`).
- **Focus Rings**: Interactive elements utilize high-contrast 2px solid `#3b82f6` rings on focus.

### Named Rules
**The Border-Over-Shadow Rule.** Never use box-shadow for container boundaries. Use 1px solid `#334155` borders to define control regions.

**The Drawer-First Rule.** Never launch full-screen modal overlays to display issue details or stack traces. Use right-hand sliding drawers or expandable inline rows to preserve context during triage.

## 5. Components

### Buttons
- **Shape:** `4px` radius.
- **Primary:** Background `#3b82f6`, text `#f8fafc`, padding `6px 12px`, font size `0.875rem` medium.
- **Hover / Focus:** Hover background `#1d4ed8`, 2px blue focus outline.

### Status & Severity Badges
- **Shape:** `4px` radius, inline-flex, uppercase monospace text (`0.75rem`).
- **Critical Badge:** Background `rgba(239, 68, 68, 0.15)`, text `#ef4444`, border `1px solid rgba(239, 68, 68, 0.3)`.
- **Warning Badge:** Background `rgba(245, 158, 11, 0.15)`, text `#f59e0b`, border `1px solid rgba(245, 158, 11, 0.3)`.
- **Resolved Badge:** Background `rgba(16, 185, 129, 0.15)`, text `#10b981`, border `1px solid rgba(16, 185, 129, 0.3)`.
- **Ignored Badge:** Background `rgba(148, 163, 184, 0.15)`, text `#94a3b8`, border `1px solid rgba(148, 163, 184, 0.3)`.

### Data Tables & Log Rows
- **Header:** Background `#1e293b`, uppercase mono text, border-bottom `1px solid #334155`.
- **Row:** Height `36px` compact, hover background `#334155/30`, border-bottom `1px solid #334155/50`.

### Stack Trace Viewer
- **Style:** Monospace `#f8fafc` text on `#0f172a` background with line numbers and expandable frame drawers.

## 6. Do's and Don'ts

### Do:
- **Do** maintain high line density (36px–40px table row height) so more incidents fit on screen without scrolling.
- **Do** use strict monospace formatting for error signatures, stack traces, and IDs.
- **Do** use context-preserving drawers and inline expandable rows over disruptive modals.

### Don't:
- **Don't** use modal dialogs for issue inspection—always use a side drawer or inline detail panel.
- **Don't** use decorative text gradients (`background-clip: text`), hero-metric cards, or blurs.
- **Don't** use colored accent side-stripes on cards or alert notifications.
- **Don't** hide critical error context or stack frames behind multi-click pagination when scrolling drawers can show full context.
