package service

import (
	"context"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"google.golang.org/protobuf/proto"
)

type IngestService struct {
	publisher *nats.Publisher
}

func NewIngestService(publisher *nats.Publisher) *IngestService {
	return &IngestService{publisher: publisher}
}

func (s *IngestService) Ingest(ctx context.Context, payload *validation.ErrorPayload) error {
	event := mapping.MapPayloadToEvent(payload)

	data, err := proto.Marshal(event)
	if err != nil {
		return err
	}

	return s.publisher.Publish(ctx, data)
}
