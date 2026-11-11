# Sentinel Constitution

## 1. Project Identity
- **Name**: Sentinel
- **Purpose**: A minimalistic system to track errors for backend services.
- **Scope**: Monorepo consisting of Golang background workers and a SvelteKit dashboard.

## 2. Engineering Philosophy
- **Domain-Driven**: We prioritize the integrity of the domain model. Code should reflect the business language and logic.
- **Explicit over Implicit**: We prefer explicit contracts (Protos) and clear patterns over "magic" frameworks.
- **Reliability by Default**: As an error tracking system, our own reliability is paramount. Async processing and robust validation are core to our identity.

## 3. Security Expectations
- **Data Integrity**: Ensure that ingested error data is sanitized and stored securely.
- **Least Privilege**: Workers and apps should only have access to the resources they strictly need.
- **Contract Enforcement**: Use Protos to ensure that data entering and moving through the system is valid and safe.

## 4. Testing Expectations
- **TDD Preferred**: We encourage writing tests before or alongside implementation.
- **Contract Testing**: Protos must be verified to ensure compatibility between workers and the dashboard.
- **Integration over Unit**: While unit tests are valuable for complex logic, integration tests between modules are essential for ensuring system flow.

## 5. Documentation Standards
- **Living Documentation**: The Constitution and Architecture Constitution are the primary sources of truth.
- **Code as Documentation**: Clean, well-named symbols and explicit types (Go/TS) should make logic discoverable.
- **Architectural Decision Records (ADR)**: Significant changes to the system should be documented as ADRs.

## 6. Review Process
- **Architecture First**: Code reviews must first verify alignment with the Architecture Constitution.
- **P0 Enforcement**: PRs with blocking violations (P0) should not be merged.
- **Collaborative Growth**: Reviews are opportunities to refine patterns and shared understanding.

## 7. High-Level Architecture Intent
- Our architecture is designed for **Scalability** and **Maintainability** through a **Modular Monolith** approach.
- Enforceable rules regarding layer boundaries and data access are defined in `.specify/memory/architecture_constitution.md`.

## 8. Governance and Evolution Policy
- **Intentional Change**: Architecture evolution follows a **Proposal-Based** model.
- **Constitution as Code**: Changes to the system's governance must be reflected in this file or the Architecture Constitution through an approved PR.

**Version**: 1.0.0 | **Ratified**: 2026-05-09 | **Last Amended**: 2026-05-09
