# Data Model: Sentinel Error Service

## Entities

### `projects`
| Field | Type | Description |
|-------|------|-------------|
| id | UUID (PK) | Unique ID |
| name | String | Project name |
| api_key | String (Indexed) | Used by SDKs for ingestion |
| created_at | Timestamp | |

### `issues`
| Field | Type | Description |
|-------|------|-------------|
| id | UUID (PK) | Unique ID |
| project_id | UUID (FK) | Reference to projects |
| fingerprint | String (Indexed) | Unique hash of the error |
| message | Text (Fulltext Indexed) | Normalized error message |
| error_class | String | e.g., pq.Error, RuntimeError |
| status | Enum | Open, Resolved, Ignored |
| first_seen | Timestamp | |
| last_seen | Timestamp | |
| count | BigInt | Total occurrences |

### `error_occurrences`
| Field | Type | Description |
|-------|------|-------------|
| id | UUID (PK) | Unique ID |
| issue_id | UUID (FK) | Reference to issues |
| environment | String | production, staging, etc. |
| platform | String | golang, ruby, etc. |
| stacktrace | JSONB | Structured stack trace |
| metadata | JSONB | Raw arbitrary metadata (other fields) |
| created_at | Timestamp | |

### `error_search_index`
| Field | Type | Description |
|-------|------|-------------|
| occurrence_id | UUID (PK, FK) | Reference to error_occurrences |
| user_id | String (Indexed) | Common metadata: User identifier |
| tenant_id | String (Indexed) | Common metadata: Tenant identifier |
| trace_id | String (Indexed) | Common metadata: Distributed trace ID |
| span_id | String | Common metadata: Span ID |
| request_id | String (Indexed)| Common metadata: Request identifier |

## Relationships
- A **Project** has many **Issues**.
- An **Issue** has many **Error Occurrences**.
- An **Error Occurrence** has one **Error Search Index** record (for optimized searching).
