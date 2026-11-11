# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - 2026-04-17

### Added

- `speckit.ripple.scan` command — analyze implementation for untested side effects across 9 categories
- `speckit.ripple.resolve` command — interactively resolve findings with multiple fix strategies and tradeoff analysis; generates `ripple-fixes.md` for `/speckit.implement` integration
- `speckit.ripple.check` command — re-verify findings and track resolution status
- `after_implement` hook — prompt to scan after implementation completes
- 9 analysis categories: Data Flow, State & Lifecycle, Interface Contract, Resource & Performance, Concurrency, Distributed Coordination, Configuration & Environment, Error Propagation, Observability
- Severity levels: CRITICAL, WARNING, INFO
- Incremental scan support (`--diff` flag)
- Fix-induced side effect detection in check command
- Coverage Gap Matrix in report output