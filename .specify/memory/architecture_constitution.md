# Sentinel Architecture Constitution

## 1. Architecture Style
- **Style**: Modular Monolith
- **Structure**: Monorepo with dedicated applications (`apps/`) and shared packages (`packages/`).
- **Isolation**: High. Modules must communicate via established contracts and shared packages.

## 2. Layer Boundaries
- **Entry Layer**: `apps/ingestor-go`, `apps/processor-go`, `apps/dashboard-web`.
- **Contract Layer**: `packages/proto`. Defines the "source of truth" for cross-module communication.
- **Shared Layer**: `packages/shared-go`, `packages/tailwind-config`. Common utilities and configurations.
- **Domain Layer**: Contained within each application/module, focusing on business logic.

## 3. Business Logic Placement
- **Domain Models**: Business logic must reside primarily within domain entities and models.
- **Thin Handlers**: Handlers (Go) and Loaders/Actions (SvelteKit) must remain thin, delegating all logic to domain models or services.
- **No Leakage**: Transport concerns (HTTP, GRPC) must not leak into domain logic.

## 4. Contracts & Validation
- **Source of Truth**: All cross-module and API contracts must be defined in `packages/proto`.
- **Validation Strategy**:
    - **Go**: Use Proto-based validation (e.g., `proto-gen-validate`) or manual checks within domain models.
    - **SvelteKit**: Use `Zod` for runtime validation of inputs (Actions/Loaders).
- **Response Structure**: Adhere to Proto/GRPC style for consistency across all workers and the dashboard.

## 5. Data Access Rules
- **Pattern**: CQRS Lite.
- **Separation**: Distinguish between read and write operations.
- **Access Control**: Application layers must use the CQRS pattern; direct database access skipping this pattern is prohibited.

## 6. Async & Integration Rules
- **Queue-First**: All non-read operations should be handled asynchronously via background workers (`ingestor-go`, `processor-go`).
- **Consistency**: Use the queue system to ensure eventual consistency and system resilience.

## 7. Module Boundaries
- **Contract-Based**: Imports between `apps/` and `packages/` must be restricted to shared libraries and proto definitions.
- **No Direct Imports**: Apps must not directly import internals from other apps.

## 8. Framework-Specific Architecture Rules
- **Go Workers**: Focus on concurrency safety and efficient resource usage in domain models.
- **SvelteKit**: Ensure clear separation between Server Actions/Loaders and business logic.

## 9. Blocking Architecture Violations (P0)
- **Boundary Violation**: Direct cross-module imports skipping contracts or shared packages.
- **Logic Leakage**: Business logic implemented in handlers, controllers, or transport layers.
- **Unvalidated Input**: Missing or bypassed input validation (Proto/Zod).
- **Pattern Deviation**: Direct database access skipping the CQRS Lite pattern.

## 10. Architecture Evolution Policy
- **Proposal-Based**: All architectural changes must be proposed and documented before implementation.
- **Drift Handling**: Detected architectural drift will trigger a mandatory review and potential Constitution update.
- **Refactor First**: Significant architectural shifts require a dedicated refactoring phase.

## 11. Refactor & Drift Handling
- Automated tools (Architecture Guard) will scan for violations during the planning and implementation phases.
- PRs containing P0 violations will be blocked until remediated or the Constitution is updated.
