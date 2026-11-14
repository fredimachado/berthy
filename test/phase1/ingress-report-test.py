import importlib.util
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location('report', Path(__file__).with_name('ingress-report.py'))
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)

class TestReport(unittest.TestCase):
    def test_arrival_before_health_is_failure(self):
        self.assertEqual(report.premature([{'id': 'abc', 'time': '2026-01-01T00:00:01Z'}], {'abc': '2026-01-01T00:00:02Z'}), 1)

    def test_missing_health_is_not_safe(self):
        self.assertEqual(report.premature([{'id': 'abc', 'time': '2026-01-01T00:00:01Z'}], {}), 1)

    def test_arrival_after_health_is_safe(self):
        self.assertEqual(report.premature([{'id': 'abc', 'time': '2026-01-01T00:00:03Z'}], {'abc': '2026-01-01T00:00:02Z'}), 0)

    def test_nanosecond_timestamp_on_python_38(self):
        self.assertEqual(report.timestamp('2026-01-01T00:00:01.123456789Z').microsecond, 123456)

if __name__ == '__main__': unittest.main()
