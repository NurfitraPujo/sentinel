package service

import (
	"errors"
	"strings"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/jackc/pgx/v5/pgconn"
)

// classifyStoreError decides whether err (as returned from a store.* call)
// should be treated as retryable (transient infrastructure trouble — DB
// down, network partition) or permanent (the message itself can never
// succeed, no matter how many times it is redelivered).
//
// This distinction is what the NATS Subscriber (packages/shared-go/nats)
// uses to dead-letter permanent failures immediately instead of consuming
// the message's whole MaxDeliver budget on redeliveries that are certain to
// fail identically every time (VERIFIED_STATE.md S13 / P4-4: "distinguish
// retryable (DB down) from permanent (malformed / constraint violation)
// failures — a permanent failure must not consume all its retries").
//
// A Postgres error is treated as permanent when it is an integrity
// constraint violation (SQLSTATE class 23 — foreign key, unique, not-null,
// check) or a data exception (class 22 — e.g. a project_id that is not a
// valid UUID). Anything else — including connection failures, which do not
// come back as *pgconn.PgError at all — is left retryable.
func classifyStoreError(err error) error {
	if err == nil {
		return nil
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if strings.HasPrefix(pgErr.Code, "23") || strings.HasPrefix(pgErr.Code, "22") {
			return nats.Permanent(err)
		}
	}

	return err
}

// classifyProjectLookupError additionally treats a "project not found"
// result as permanent: retrying the same event will not make a project
// that does not exist appear. This is deliberately its own function (rather
// than folded into classifyStoreError) because "not found" here is
// application-level (constructed with fmt.Errorf in store.go), not a
// *pgconn.PgError.
func classifyProjectLookupError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "project not found") {
		return nats.Permanent(err)
	}
	return classifyStoreError(err)
}
