package service

import (
	"context"
	"fmt"

	"buf.build/go/protovalidate"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"google.golang.org/protobuf/proto"
)

type IngestService struct {
	publisher *nats.Publisher
	validator protovalidate.Validator
}

func NewIngestService(publisher *nats.Publisher) (*IngestService, error) {
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize validator: %w", err)
	}
	return &IngestService{
		publisher: publisher,
		validator: v,
	}, nil
}

func (s *IngestService) Ingest(ctx context.Context, payload *validation.ErrorPayload) error {
	event := mapping.MapPayloadToEvent(payload)

	if err := s.validator.Validate(event); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	data, err := proto.Marshal(event)
	if err != nil {
		return err
	}

	return s.publisher.Publish(ctx, data)
}
