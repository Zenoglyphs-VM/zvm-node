"""Memory-only scheduler controls; these do not execute builds or native tools."""
import importlib.util
import json
from pathlib import Path
import sys
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('market_build', Path(__file__).with_name('ci-market-build.py'))
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)

class Fake:
    def __init__(self, batches, free=None):
        self.batches = list(batches)
        self.started = []
        self.cancelled = 0
        self.joined = False
        self.clock = 0
        self.frees = list(free or [1000])
        self.raw = []
    def now(self): return self.clock
    def free(self): return self.frees.pop(0) if len(self.frees) > 1 else self.frees[0]
    def start(self, role, argv):
        self.started.append(role)
        return len(self.started)
    def events(self):
        self.clock += 1
        return self.batches.pop(0) if self.batches else []
    def emit(self, role, raw): self.raw.append((role, raw))
    def assert_joined(self): self.joined = True
    def cancel(self): self.cancelled += 1

PLAN = [[('go', ['declared-fake'])], [('install', ['declared-fake']), ('build', ['declared-fake'])]]

class Controls(unittest.TestCase):
    def run_fake(self, fake, **options):
        return build.run(PLAN, fake, floor=10, reservation=20, **options)
    def test_overlap_and_dependency_join(self):
        fake = Fake([[(2, 'exit', 0)], [(3, 'exit', 0)], [(1, 'exit', 0)]])
        result = self.run_fake(fake)
        self.assertEqual(fake.started, ['go', 'install', 'build'])
        self.assertEqual(result['maximumActiveCommandGroups'], 2)
        self.assertEqual(result['commandRoles'], ['install', 'build', 'go'])
        self.assertTrue(fake.joined)
        self.assertEqual(fake.cancelled, 0)
    def test_go_failure_cancels_other_lane_without_starting_build(self):
        fake = Fake([[(1, 'exit', 7)]])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.started, ['go', 'install'])
        self.assertEqual(fake.cancelled, 1)
        self.assertFalse(fake.joined)
    def test_install_failure_prevents_dependent_build(self):
        fake = Fake([[(2, 'exit', 1)]])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.started, ['go', 'install'])
        self.assertEqual(fake.cancelled, 1)
    def test_deadline_joins_active_work(self):
        fake = Fake([[], [], []])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake, timeout=2)
        self.assertEqual(fake.cancelled, 1)
    def test_missing_space_prevents_any_launch(self):
        fake = Fake([], free=[29])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.started, [])
        self.assertEqual(fake.cancelled, 0)
    def test_live_floor_refusal_cancels_owned_work(self):
        fake = Fake([[]], free=[1000, 1000, 9])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.cancelled, 1)
    def test_reserved_growth_refusal_cancels_owned_work(self):
        fake = Fake([[]], free=[1000, 1000, 979])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.cancelled, 1)
    def test_output_is_bounded_and_counted_before_emission(self):
        fake = Fake([[(1, 'output', b'ab')], [(1, 'output', b'cd')]])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake, output_cap=3)
        self.assertEqual(fake.raw, [('go', b'ab')])
        self.assertEqual(fake.cancelled, 1)
    def test_unowned_event_is_refused(self):
        fake = Fake([[(4, 'exit', 0)]])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.cancelled, 1)
    def test_duplicate_terminal_is_refused(self):
        fake = Fake([[(1, 'exit', 0), (1, 'exit', 0)]])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.cancelled, 1)
    def test_boolean_exit_is_not_success(self):
        fake = Fake([[(1, 'exit', False)]])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.cancelled, 1)
    def test_unknown_event_is_refused(self):
        fake = Fake([[(1, 'unknown', 0)]])
        with self.assertRaises(build.BuildFailure): self.run_fake(fake)
        self.assertEqual(fake.cancelled, 1)
    def test_keyboard_interrupt_cancels_owned_work(self):
        fake = Fake([])
        def interrupt(): raise KeyboardInterrupt
        fake.events = interrupt
        with self.assertRaises(KeyboardInterrupt): self.run_fake(fake)
        self.assertEqual(fake.cancelled, 1)
    def test_duplicate_roles_are_refused_before_launch(self):
        fake = Fake([])
        with self.assertRaises(build.BuildFailure): build.run([[('same', [])], [('same', [])]], fake)
        self.assertEqual(fake.started, [])
    def test_all_five_original_build_commands_and_limits(self):
        plan = build.plans('/declared/temporary/output')
        self.assertEqual([role for commands in plan for role, _ in commands], ['relayer','nft-install','karum-install','nft-build','karum-build'])
        self.assertEqual(plan[0][0][1], ['go','-C','.market-browser/zvm-relayer','build','-p=2','-trimpath','-buildvcs=true','-o','/declared/temporary/output','./cmd/zvm-relayer'])
        for role, argv in plan[1][:2]:
            self.assertEqual(argv[:4], ['npm','ci','--ignore-scripts','--include=dev'])
        self.assertEqual(build.FLOOR, 10 << 30)
        self.assertEqual(build.RESERVATION, 8 << 30)
        self.assertEqual(build.TIMEOUT, 240)
    def test_five_commands_finish_once_with_complete_outputs(self):
        fake = Fake([[(1,'output',b'go'),(2,'exit',0)],[(3,'exit',0)],[(4,'exit',0)],[(5,'exit',0),(1,'exit',0)]])
        value = build.run(build.plans('/declared/temporary/output'), fake, floor=10, reservation=20)
        self.assertEqual(value['commandCount'], 5)
        self.assertEqual(value['outputBytesByRole']['relayer'], 2)
        self.assertTrue(value['allOwnedGroupsJoined'])
    def test_registration_failure_keeps_the_started_child_owned(self):
        class Pipe:
            def fileno(self): return 123
        class Child:
            stdout = Pipe()
        class Selector:
            def register(self, *args): raise OSError('declared registration failure')
        backend = build.Processes.__new__(build.Processes)
        backend.children = {}; backend.serial = 0; backend.selector = Selector()
        with patch.object(build.subprocess, 'Popen', return_value=Child()), patch.object(build.os, 'set_blocking'):
            with self.assertRaises(OSError): backend.start('relayer', ['declared-fake'])
        self.assertEqual(list(backend.children), [1])
        self.assertFalse(backend.children[1]['registered'])
    def test_real_backend_refuses_success_with_an_unjoined_child(self):
        backend = build.Processes.__new__(build.Processes)
        backend.children = {1: {'declared': 'unjoined'}}
        with self.assertRaises(build.BuildFailure): backend.assert_joined()

if __name__ == '__main__':
    result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(Controls))
    assert result.testsRun == 18
    print('MARKET-INDEPENDENT-BUILD-CONTROLS ' + json.dumps({'cases': result.testsRun, 'pass': result.wasSuccessful(),
          'memoryOnly': True, 'nativeBuildsOrBrowsersOrNetwork': 0}, sort_keys=True))
    sys.exit(0 if result.wasSuccessful() else 1)
