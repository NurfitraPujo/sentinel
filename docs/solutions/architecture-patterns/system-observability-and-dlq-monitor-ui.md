---
title: System Observability Dashboard and DLQ Monitoring UI Architecture Pattern
date: 2026-08-01
category: docs/solutions/architecture-patterns
module: dashboard-web
problem_type: architecture_pattern
component: frontend_stimulus
severity: medium
applies_when:
  - Building administrative operational health and telemetry monitoring views
  - Integrating SvelteKit frontend server loaders with Go backend health & DLQ endpoints
  - Implementing high-density data tables with slide-out JSON drawer inspectors
tags:
  - observability
  - dlq-monitor
  - sveltekit
  - telemetry
  - jetstream
---

# System Observability Dashboard and DLQ Monitoring UI Architecture Pattern

## Context
Sentinel services (`ingestor-go` and `processor-go`) export Prometheus metrics via `/metrics` and dead-letter queue (DLQ) backlog status via `/health` and `/dlq`. To give SREs and administrators visibility into operational health, queue depth, latency percentiles, and dead-lettered events, `apps/dashboard-web` required an administrative health surface mounted at both `/settings/observability` and `/[orgSlug]/settings/observability`.

## Guidance

### 1. Constant-Time O(1) Telemetry Proxying & Health Endpoint Design
To ensure health checks and observability endpoints never degrade under high DLQ backlogs (e.g. thousands of parked messages), the backend processor (`apps/processor-go`) maintains an $O(1)$ constant-time sampling strategy:
- `GET /health`: Exposes database ping status, DLQ depth, publish failure counter, and oldest parked message age/class.
- `GET /dlq`: Returns DLQ summary stats and the single oldest parked message as an actionable sample item (`O(1)` JetStream `StreamInfo` + `GetMsg`), preserving system performance without running costly queue-wide scans on HTTP request threads.

### 2. Resilient SvelteKit Server-Side Proxying
SvelteKit server loaders (`+page.server.ts`) proxy backend HTTP requests server-side rather than fetching directly from the browser:
- Environment variable configuration (`PROCESSOR_HEALTH_URL`, `INGESTOR_HEALTH_URL`) with fallback defaults for local execution.
- Graceful degradation: if a service or endpoint is unreachable, server loaders return `status: 'offline'` structured fallbacks rather than throwing unhandled 500 errors.

```typescript
// apps/dashboard-web/src/routes/settings/observability/+page.server.ts
export const load: PageServerLoad = async ({ fetch }) => {
	const processorUrl = process.env.PROCESSOR_HEALTH_URL || 'http://localhost:8081';
	const ingestorUrl = process.env.INGESTOR_HEALTH_URL || 'http://localhost:8080';

	let processorData = { status: 'unknown', database: 'unknown', dlq_depth: 0, dlq_publish_failures: 0, dlq_threshold: 25, dlq_stale_after_seconds: 3600 };
	let ingestorData = { status: 'unknown' };
	let dlqData = { total_depth: 0, publish_failures: 0, items: [] as DLQItem[] };

	try {
		const res = await fetch(`${processorUrl}/health`);
		if (res.ok) processorData = await res.json();
	} catch (e) {
		processorData = { ...processorData, status: 'offline', database: 'unreachable' };
	}

	try {
		const res = await fetch(`${processorUrl}/dlq`);
		if (res.ok) dlqData = await res.json();
	} catch (e) {}

	return { observability: { processor: processorData, ingestor: ingestorData, dlq: dlqData, fetchedAt: new Date().toISOString() } };
};
```

### 3. Svelte 5 Reactive State and Side-Drawer Payload Inspector
For high-density tables displaying complex JSON event payloads:
- Use `$derived.by()` reactive getters for formatting raw JSON payloads to prevent inline IIFE re-evaluation overhead during template rendering.
- Present summary metadata (Timestamp, Error Class badge, Event ID, Retry Count) in the main table grid.
- Provide a non-modal slide-out side drawer with tabbed code blocks for raw payload viewing, stack traces, and 1-click clipboard actions.

```svelte
<script lang="ts">
	let selectedItem = $state<DLQItem | null>(null);

	let formattedPayload = $derived.by(() => {
		if (!selectedItem) return '';
		try {
			return JSON.stringify(JSON.parse(selectedItem.raw_payload), null, 2);
		} catch (e) {
			return selectedItem.raw_payload;
		}
	});
</script>

{#if selectedItem}
	<div class="drawer-backdrop" onclick={closeDrawer}>
		<div class="drawer-panel" onclick={(e) => e.stopPropagation()}>
			<pre class="json-code"><code>{formattedPayload}</code></pre>
		</div>
	</div>
{/if}
```

## Why This Matters
- Prevents $O(N)$ HTTP request delays when queues accumulate dead-lettered events.
- Protects administrative dashboards from collapsing when backend services are temporarily unreachable.
- Delivers a high-density, product-register compliant operational UI for rapid incident triage.

## When to Apply
- When creating administrative system monitoring pages or telemetry dashboards.
- When surfacing NATS JetStream or queue backlog inspection tools in web UIs.

## Related
- `docs/todos/13-dashboard-observability-and-dlq-monitor-ui.md`
- `apps/processor-go/main.go`
- `apps/dashboard-web/src/routes/settings/observability/+page.svelte`
