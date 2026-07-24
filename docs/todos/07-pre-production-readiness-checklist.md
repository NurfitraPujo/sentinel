# TODO 07: Pre-Production Readiness Checklist

## Priority: Recommended
## Status: Pending

### Overview
Action items to execute immediately before connecting live production systems to Sentinel.

### Checklist
- [ ] **SDK Retry & Async Buffer**: Ensure client application error submission runs asynchronously off the main request thread with retry backoff.
- [ ] **NATS Storage Volume**: Configure persistent SSD storage for NATS JetStream stream data (`ERROR_EVENTS`).
- [ ] **Ingestor Rate Limiting**: Enable per-project rate limiting on `apps/ingestor-go` to prevent database and stream overload during outages.
- [ ] **Database Connection Pooling**: Deploy PgBouncer in front of PostgreSQL to handle high concurrent worker connections.
- [ ] **Database Backup Strategy**: Configure automated PostgreSQL WAL archiving and daily snapshot backups.
- [ ] **SSL / TLS Termination**: Enforce TLS on `ingestor-go` endpoints and dashboard ingress.
