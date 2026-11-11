# Product

## Register

product

## Users

Backend developers, SREs, DevOps engineers, engineering managers, and support teams. Context: Monitoring production backend errors, analyzing error occurrences/stack traces, configuring alerts, and creating, managing, and tracking issue lifecycles (including manually reported user issues).

## Product Purpose

Sentinel is a minimalistic, high-performance error tracking system. It ingests backend service error events via a Go SDK / HTTP ingestor, processes them asynchronously through NATS JetStream into PostgreSQL, and provides a web dashboard (`dashboard-web`) to render issues, occurrences, organization tenancy, key management, and alerting configurations.

## Brand Personality

- **Dense & Efficient**: High information density tailored for fast scanning during active incidents or routine triage.
- **Functional Clarity**: Sharp visual hierarchy inspired by tools like Linear and Datadog; utility and precision over decorative fluff.
- **Dependable & Calm**: Clear operational status, calm alert indicators, and predictable controls under high operational stress.

## Anti-references

- Low-density consumer SaaS templates with giant empty white margins and decorative cards.
- Gimmicky glassmorphism, floating blurs, or gradient text decorations.
- Over-animated transitions that impede rapid navigation during incident triage.
- Confusing modal-heavy navigation flows when inline or side-panel drawers are more context-preserving.

## Design Principles

1. **Information Density First**: Prioritize data scannability, compact tables, and clear status badges so engineers can assess issue impact in seconds.
2. **Context-Preserving Workflows**: Prefer side drawers, split views, and inline expansion over full-page jumps or disruptive modal dialogs.
3. **Calm Under Fire**: Use color strictly for operational state (severities, status, trend alerts) rather than visual noise.
4. **Keyboard & Action Efficiency**: Quick filters, easy copy-to-clipboard for stack traces/IDs, and responsive UI feedback.

## Accessibility & Inclusion

- WCAG 2.1 Level AA compliance.
- High contrast for status indicators and severity badges.
- Accessible keyboard navigation and visible focus rings across tables, search inputs, and action buttons.
- Full respect for `prefers-reduced-motion`.
