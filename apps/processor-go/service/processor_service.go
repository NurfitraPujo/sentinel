package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/degradation"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/indexer"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ProcessorService struct {
	db          *pgxpool.Pool
	store       store.IssueStore
	indexer     *indexer.Indexer
	degradation *degradation.GracefulDegradation
}

func NewProcessorService(db *pgxpool.Pool) *ProcessorService {
	return &ProcessorService{
		db:      db,
		store:   store.NewStore(db),
		indexer: indexer.NewIndexer(db),
		degradation: degradation.NewGracefulDegradation(func(ctx context.Context) bool {
			return db.Ping(ctx) == nil
		}),
	}
}

func (s *ProcessorService) ProcessEvent(ctx context.Context, data []byte) error {
	if !s.degradation.CheckAndBuffer(ctx, data) {
		log.Printf("Event buffered due to database unavailability")
		return nil
	}

	return s.processEventInternal(ctx, data)
}

func (s *ProcessorService) processEventInternal(ctx context.Context, data []byte) error {
	evt, err := event.Deserialize(data)
	if err != nil {
		log.Printf("Failed to deserialize event: %v", err)
		return err
	}

	log.Printf("Processing event: project=%s, error_class=%s, fingerprint=%s",
		evt.ProjectKey, evt.ErrorClass, evt.Fingerprint)

	projectID, err := s.store.GetProjectByKey(ctx, evt.ProjectKey)
	if err != nil {
		log.Printf("Failed to get project: %v", err)
		return err
	}

	issue := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: evt.Fingerprint,
		Message:     evt.Message,
		ErrorClass:  evt.ErrorClass,
		Status:      "open",
		FirstSeen:   evt.Timestamp,
		LastSeen:    evt.Timestamp,
	}

	if err := s.store.UpsertIssue(ctx, issue); err != nil {
		log.Printf("Failed to upsert issue: %v", err)
		return err
	}

	s.store.PersistAuditLog(ctx, &store.AuditLog{
		ID:           uuid.New().String(),
		Action:       "issue_upserted",
		ResourceType: "issue",
		ResourceID:   &issue.ID,
		ActorID:      "processor-go",
		Metadata:     []byte(fmt.Sprintf(`{"fingerprint": "%s", "project_id": "%s"}`, issue.Fingerprint, issue.ProjectID)),
	})

	issueID, err := s.store.GetIssueIDByFingerprint(ctx, projectID, evt.Fingerprint)
	if err != nil {
		log.Printf("Failed to get issue ID: %v", err)
		return err
	}

	stacktraceJSON, _ := json.Marshal(evt.Stacktrace)
	metadataJSON, _ := json.Marshal(evt.Metadata)

	occ := &store.ErrorOccurrence{
		ID:          uuid.New().String(),
		IssueID:     issueID,
		Environment: evt.Environment,
		Platform:    evt.Platform,
		Stacktrace:  stacktraceJSON,
		Metadata:    metadataJSON,
		TraceID:     evt.TraceID,
		SpanID:      evt.SpanID,
		CreatedAt:   evt.Timestamp,
	}

	if err := s.store.InsertOccurrence(ctx, occ); err != nil {
		log.Printf("Failed to insert occurrence: %v", err)
		return err
	}

	s.store.PersistAuditLog(ctx, &store.AuditLog{
		ID:           uuid.New().String(),
		Action:       "occurrence_created",
		ResourceType: "error_occurrence",
		ResourceID:   &occ.ID,
		ActorID:      "processor-go",
		Metadata:     []byte(fmt.Sprintf(`{"issue_id": "%s", "environment": "%s"}`, occ.IssueID, occ.Environment)),
	})

	searchEntry := indexer.ExtractSearchFields(evt.Metadata)
	searchEntry.OccurrenceID = occ.ID
	if searchEntry.TraceID == "" {
		searchEntry.TraceID = evt.TraceID
	}
	if searchEntry.SpanID == "" {
		searchEntry.SpanID = evt.SpanID
	}

	if err := s.indexer.IndexOccurrence(ctx, searchEntry); err != nil {
		log.Printf("Failed to index occurrence: %v", err)
	}

	s.degradation.Flush(ctx, func(eventData []byte) error {
		return s.processEventInternal(ctx, eventData)
	})

	return nil
}
