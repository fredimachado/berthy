## 2026-11-22: Subscribe to events that wake checks
- Rule: In the Docker event hub, subscribe to each container action that wakes a monitor check, because Docker filters events before the monitor sees them.
- Level: 2; a filter-aware hub-to-monitor test detects a missing subscription without coupling the two packages.
- Enforced by: TestContainerStartsWakeDownCheckThroughHub.
- Proven: The test failed for both start and restart before the subscription fix.
- Existing violations: 1 filter fixed, 0 baselined.
- Follow-up: none.
