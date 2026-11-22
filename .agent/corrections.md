## 2026-11-22: Subscribe to events that wake checks
- Rule: In the Docker event hub, subscribe to each container action that wakes a monitor check, because Docker filters events before the monitor sees them.
- Level: 2; a filter-aware hub-to-monitor test detects a missing subscription without coupling the two packages.
- Enforced by: TestContainerStartsWakeDownCheckThroughHub.
- Proven: The test failed for both start and restart before the subscription fix.
- Existing violations: 1 filter fixed, 0 baselined.
- Follow-up: none.

## 2026-11-22: Classify replaced services during compensation
- Rule: When restoring an update, distinguish a confirmed service ID mismatch from an inspection failure, because a replaced service was changed outside Berthy.
- Level: 2; a behavior test detects the wrong category and checks the replacement is untouched.
- Enforced by: TestCompensationReportsReplacedService.
- Proven: The test failed with the generic category before the fix.
- Existing violations: 1 recovery check fixed, 0 baselined.
- Follow-up: none.

## 2026-11-22: Keep private Docker errors out of recovery output
- Rule: In compensation failure reporting, log and store only fixed categories, because Docker errors may contain secrets.
- Level: 2; the operation test checks the log, all diagnostic fields, and all operation fields for an injected private error.
- Enforced by: TestFailedCompensationRecordsCategoryAndKeepsLock.
- Proven: The strengthened test passed on the existing behavior and failed when each of the three outputs was temporarily mutated to leak the error.
- Existing violations: 0 found in compensation reporting, 0 baselined.
- Follow-up: none.
