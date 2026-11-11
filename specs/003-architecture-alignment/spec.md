# Spec: Architecture Alignment & Completion

## Goal
Bring the Sentinel codebase into full alignment with its Constitution and complete the pending Error Service implementations.

## Requirements
1. **Validation**: Use `protovalidate-go` in Ingestor.
2. **Security**: Mask `Message` field in Processor.
3. **Contract**: Update `error_event.proto` with fingerprinting fields.
4. **Clean Architecture**: Move Processor logic to service layer.
5. **Feature Completion**: Implement SMTP UI, persistence, Redis rate limiting, and Audit persistence.
