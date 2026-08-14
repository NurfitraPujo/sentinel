import { z } from 'zod';
import { Document } from 'yaml';
import { agentApiRegistry, type RegistryEntry } from './registry';

/**
 * N6: converts `agentApiRegistry` (registry.ts) into an OpenAPI 3.1 document and serializes it to
 * YAML. Pure functions, no filesystem/process side effects -- `scripts/generate-agent-openapi.ts`
 * (the `pnpm openapi:agent` CLI entry) and `openapi-drift.test.ts` both import this module so the
 * exact same code path produces the committed file and the in-memory comparison.
 *
 * The output MUST be byte-for-byte deterministic across runs given an unchanged registry
 * (openapi-drift.test.ts and the "second generate is a no-op" determinism check both depend on
 * this) -- every object below is built with an EXPLICIT, fixed key order, and the zod->JSON-Schema
 * conversion never iterates a `Map`/`Set`/object whose key order isn't already fixed by us.
 *
 * Deliberately hand-rolled rather than `@asteasolutions/zod-to-openapi`: this only needs to cover
 * the small subset of zod used in schemas.ts (object/strict/array/string/number/boolean/enum/
 * literal/nullable/optional/union/record/unknown/coerce.date/coerce.number), and hand-rolling it
 * keeps determinism trivially provable by inspection instead of trusting a third-party library's
 * internal key ordering across versions.
 */

type JsonSchema = Record<string, unknown>;

function unwrap(schema: z.ZodTypeAny): { inner: z.ZodTypeAny; nullable: boolean; optional: boolean } {
	let inner = schema;
	let nullable = false;
	let optional = false;
	for (;;) {
		if (inner instanceof z.ZodOptional) {
			optional = true;
			inner = inner._def.innerType;
			continue;
		}
		if (inner instanceof z.ZodNullable) {
			nullable = true;
			inner = inner._def.innerType;
			continue;
		}
		if (inner instanceof z.ZodDefault) {
			inner = inner._def.innerType;
			continue;
		}
		break;
	}
	return { inner, nullable, optional };
}

function zodToJsonSchema(schemaIn: z.ZodTypeAny): JsonSchema {
	const { inner, nullable } = unwrap(schemaIn);
	const base = zodTypeToJsonSchema(inner);
	if (!nullable) return base;
	// OpenAPI 3.1 / JSON Schema 2020-12: nullable is expressed via a type union.
	if (typeof base.type === 'string') {
		return { ...base, type: [base.type, 'null'] };
	}
	if (Array.isArray(base.type)) {
		return { ...base, type: [...base.type, 'null'] };
	}
	if (base.$ref || base.oneOf || base.allOf || base.anyOf) {
		return { anyOf: [base, { type: 'null' }] };
	}
	return { ...base, type: 'null' };
}

function zodTypeToJsonSchema(schema: z.ZodTypeAny): JsonSchema {
	if (schema instanceof z.ZodString) {
		const out: JsonSchema = { type: 'string' };
		for (const check of schema._def.checks ?? []) {
			if (check.kind === 'datetime') out.format = 'date-time';
			if (check.kind === 'regex') out.pattern = check.regex.source;
		}
		return out;
	}
	if (schema instanceof z.ZodNumber) {
		const out: JsonSchema = { type: 'number' };
		for (const check of schema._def.checks ?? []) {
			if (check.kind === 'int') out.type = 'integer';
			if (check.kind === 'min') out.minimum = check.value;
			if (check.kind === 'max') out.maximum = check.value;
		}
		return out;
	}
	if (schema instanceof z.ZodBoolean) return { type: 'boolean' };
	if (schema instanceof z.ZodLiteral) return { type: typeof schema._def.value, enum: [schema._def.value] };
	if (schema instanceof z.ZodEnum) return { type: 'string', enum: [...schema._def.values] };
	if (schema instanceof z.ZodUnknown || schema instanceof z.ZodAny) return {};
	if (schema instanceof z.ZodRecord) return { type: 'object', additionalProperties: true };
	if (schema instanceof z.ZodArray) {
		const out: JsonSchema = { type: 'array', items: zodToJsonSchema(schema._def.type) };
		if (schema._def.minLength) out.minItems = schema._def.minLength.value;
		if (schema._def.maxLength) out.maxItems = schema._def.maxLength.value;
		return out;
	}
	if (schema instanceof z.ZodEffects) {
		// z.coerce.date() / z.coerce.number() -- described as their target JSON wire type.
		const inputType = schema._def.schema;
		if (schema._def.effect?.type === 'preprocess') {
			try {
				const probe = schema.parse(new Date().toISOString());
				if (probe instanceof Date) return { type: 'string', format: 'date-time' };
			} catch {
				/* fall through */
			}
			try {
				const probe = schema.parse('5');
				if (typeof probe === 'number') return { type: 'number' };
			} catch {
				/* fall through */
			}
		}
		return zodToJsonSchema(inputType);
	}
	if (schema instanceof z.ZodDate) return { type: 'string', format: 'date-time' };
	if (schema instanceof z.ZodUnion) {
		return { oneOf: schema._def.options.map((opt: z.ZodTypeAny) => zodToJsonSchema(opt)) };
	}
	if (schema instanceof z.ZodObject) {
		const shape = schema._def.shape();
		const properties: JsonSchema = {};
		const required: string[] = [];
		for (const key of Object.keys(shape)) {
			const field = shape[key] as z.ZodTypeAny;
			properties[key] = zodToJsonSchema(field);
			const { optional } = unwrap(field);
			if (!optional) required.push(key);
		}
		const out: JsonSchema = { type: 'object', properties };
		if (required.length > 0) out.required = required;
		const isStrict = schema._def.unknownKeys === 'strict';
		out.additionalProperties = isStrict ? false : true;
		return out;
	}
	// Fallback: should not happen for the subset used in schemas.ts.
	return {};
}

const STATUS_DESCRIPTIONS: Record<string, string> = {
	'200': 'OK',
	'201': 'Created',
	'400': 'Bad request',
	'401': 'Unauthorized',
	'404': 'Not found',
	'409': 'Conflict',
	'413': 'Payload too large',
	'415': 'Unsupported media type',
	'503': 'Service unavailable',
};

function buildPathItem(entries: RegistryEntry[]): JsonSchema {
	const pathItem: JsonSchema = {};
	for (const entry of entries) {
		const operation: JsonSchema = {
			operationId: entry.operationId,
			summary: entry.summary,
		};

		const parameters: JsonSchema[] = [];
		if (entry.path.includes('{issueId}')) {
			parameters.push({ name: 'issueId', in: 'path', required: true, schema: { type: 'string' } });
		}
		if (entry.request?.querySchema instanceof z.ZodObject) {
			const shape = entry.request.querySchema._def.shape();
			for (const key of Object.keys(shape)) {
				const field = shape[key] as z.ZodTypeAny;
				const { optional } = unwrap(field);
				parameters.push({
					name: key,
					in: 'query',
					required: !optional,
					schema: zodToJsonSchema(field),
				});
			}
		}
		if (parameters.length > 0) operation.parameters = parameters;

		if (entry.request?.bodySchema) {
			operation.requestBody = {
				required: true,
				content: { 'application/json': { schema: zodToJsonSchema(entry.request.bodySchema) } },
			};
		}
		if (entry.path === '/api/agent/uploads' && entry.method === 'post') {
			operation.requestBody = {
				required: true,
				content: {
					'multipart/form-data': {
						schema: {
							type: 'object',
							required: ['file'],
							properties: {
								file: { type: 'string', format: 'binary' },
								filename: { type: 'string' },
							},
							additionalProperties: false,
						},
					},
				},
			};
		}

		const responses: JsonSchema = {};
		for (const status of Object.keys(entry.responses).sort()) {
			responses[status] = {
				description: STATUS_DESCRIPTIONS[status] ?? status,
				content: { 'application/json': { schema: zodToJsonSchema(entry.responses[status]) } },
			};
		}
		operation.responses = responses;

		pathItem[entry.method] = operation;
	}
	return pathItem;
}

export function generateAgentOpenApiDocument(): JsonSchema {
	const pathsByPath = new Map<string, RegistryEntry[]>();
	// Registry order defines path emission order -- deterministic, no sort needed beyond that.
	for (const entry of agentApiRegistry) {
		const list = pathsByPath.get(entry.path) ?? [];
		list.push(entry);
		pathsByPath.set(entry.path, list);
	}

	const paths: JsonSchema = {};
	for (const [pathKey, entries] of pathsByPath) {
		paths[pathKey] = buildPathItem(entries);
	}

	return {
		openapi: '3.1.0',
		info: {
			title: 'Sentinel Agent API',
			version: '1.0.0',
			description:
				'GENERATED -- do not hand-edit. Source of truth is apps/dashboard-web/src/lib/server/agent-api-spec/ ' +
				'(schemas.ts + registry.ts); regenerate with `pnpm openapi:agent` after any change there. ' +
				'docs/agents/SENTINEL_AGENT_GUIDE.md remains the prose-authoritative reference; this file is a ' +
				"machine-readable schema kept in lockstep with the route handlers under apps/dashboard-web/src/routes/api/agent/** " +
				'by openapi-drift.test.ts and completeness.test.ts.',
		},
		servers: [{ url: 'https://{host}', variables: { host: { default: 'sentinel.example.com' } } }],
		security: [{ agentKey: [] }],
		components: {
			securitySchemes: {
				agentKey: {
					type: 'http',
					scheme: 'bearer',
					bearerFormat: 'sent_agent_<64 lowercase hex chars>',
					description:
						"Org-scoped agent key. project_api_keys.scope='agent', linked to one agent, one " +
						'organization. organizationId is always derived from this credential server-side, never ' +
						'from any request parameter.',
				},
			},
		},
		paths,
	};
}

export function toYaml(doc: JsonSchema): string {
	const yamlDoc = new Document(doc);
	return yamlDoc.toString({ lineWidth: 0 });
}
