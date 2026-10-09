# Search and UI release checklist

- [x] Review and finish the FSearch-inspired name memo implementation.
- [x] Defer candidate materialization until after bounded rank selection.
- [x] Verify result/count parity, cancellation, mutation and overlay handling.
- [x] Run static analysis, Go/UI/frontend tests, race checks and CLI integration.
- [x] Commit and push reviewed search changes.
- [x] Merge `ui/minimal-prompt-polish` and fix UI integration issues.
- [x] Benchmark the combined implementation and document measured results.
- [ ] Build release binaries, pass remote CI and publish the release.
- [ ] Finish native keyboard/mouse smoke checks in an interactive desktop session;
  the desktop driver could capture the packaged UI but could not activate it.
