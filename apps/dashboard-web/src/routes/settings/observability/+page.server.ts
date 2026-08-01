import type { PageServerLoad } from './$types';

export interface DLQItem {
	sequence: number;
	event_id: string;
	org_id: string;
	project_id: string;
	error_class: string;
	error_message: string;
	failed_at: string;
	retry_attempts: number;
	raw_payload: string;
}

export interface ObservabilityData {
	processor: {
		status: string;
		database: string;
		dlq_depth: number;
		dlq_publish_failures: number;
		dlq_threshold: number;
		dlq_stale_after_seconds: number;
		dlq_oldest_age_seconds?: number;
		dlq_oldest_class?: string;
		error?: string;
	};
	ingestor: {
		status: string;
		database?: string;
		redis?: string;
		error?: string;
	};
	dlq: {
		total_depth: number;
		publish_failures: number;
		oldest_age_seconds?: number;
		oldest_class?: string;
		items: DLQItem[];
	};
	fetchedAt: string;
}

export const load: PageServerLoad = async ({ fetch }) => {
	const processorUrl = process.env.PROCESSOR_HEALTH_URL || 'http://localhost:8081';
	const ingestorUrl = process.env.INGESTOR_HEALTH_URL || 'http://localhost:8080';

	let processorData = {
		status: 'unknown',
		database: 'unknown',
		dlq_depth: 0,
		dlq_publish_failures: 0,
		dlq_threshold: 25,
		dlq_stale_after_seconds: 3600
	};

	let ingestorData = {
		status: 'unknown'
	};

	let dlqData = {
		total_depth: 0,
		publish_failures: 0,
		items: [] as DLQItem[]
	};

	try {
		const res = await fetch(`${processorUrl}/health`);
		if (res.ok) {
			processorData = await res.json();
		}
	} catch (e: any) {
		processorData = {
			...processorData,
			status: 'offline',
			database: 'unreachable'
		};
	}

	try {
		const res = await fetch(`${ingestorUrl}/health`);
		if (res.ok) {
			ingestorData = await res.json();
		}
	} catch (e: any) {
		ingestorData = {
			status: 'offline'
		};
	}

	try {
		const res = await fetch(`${processorUrl}/dlq`);
		if (res.ok) {
			dlqData = await res.json();
		}
	} catch (e: any) {
		// Degradates gracefully if endpoint unavailable
	}

	return {
		observability: {
			processor: processorData,
			ingestor: ingestorData,
			dlq: dlqData,
			fetchedAt: new Date().toISOString()
		}
	} as { observability: ObservabilityData };
};
