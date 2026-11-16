#!/usr/bin/env python3
"""Exercise routing failure and process interruption using local HTTP servers."""
import fcntl
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

SOURCE = Path(__file__).with_name("switch-traefik.py")
SPEC = importlib.util.spec_from_file_location("switch_traefik", SOURCE)
rollout = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(rollout)


class SwitchTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.route = Path(directory.name) / "active.yml"
        self.old_ready = self.new_ready = True
        self.gateway_failure = False
        self.block_gateway = False
        self.gateway_entered, self.gateway_release = threading.Event(), threading.Event()
        self.addCleanup(self.gateway_release.set)
        self.old = self.server(lambda path: self.response("a", self.old_ready, path))
        self.new = self.server(lambda path: self.response("b", self.new_ready, path))
        self.gateway = self.server(self.gateway_response)
        self.document = {"http": {"services": {"subrouter": {"loadBalancer": {
            "servers": [{"url": self.old}]}}}}}
        self.route.write_text(json.dumps(self.document))
        self.route.chmod(0o640)

    def server(self, callback):
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                status, body = callback(self.path)
                data = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                try:
                    self.wfile.write(data)
                except BrokenPipeError:
                    pass

            def log_message(self, *_):
                pass
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        server.daemon_threads = True
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return f"http://127.0.0.1:{server.server_port}"

    def response(self, identity, ready, path):
        return (200 if ready or path.endswith("health") else 503,
                {"ok": ready, "build": {"commit": identity * 12}})

    def gateway_response(self, path):
        selected = rollout.selected(json.loads(self.route.read_text()))["url"]
        if selected == self.new:
            self.gateway_entered.set()
            if self.block_gateway:
                self.gateway_release.wait(5)
            if self.gateway_failure:
                return 503, {}
        return self.response("b" if selected == self.new else "a", True, path)

    def apply(self):
        return rollout.switch(self.route, self.gateway, self.new, self.old, "b" * 40, timeout=0.25)

    def active(self):
        return rollout.selected(json.loads(self.route.read_text()))["url"]

    def command(self):
        return [sys.executable, str(SOURCE), "--route", str(self.route), "--gateway", self.gateway,
                "--candidate", self.new, "--previous", self.old, "--revision", "b" * 40, "--timeout", "10"]

    def test_verified_switch_and_repeat_are_idempotent(self):
        self.assertTrue(self.apply()["changed"])
        self.assertEqual(self.active(), self.new)
        self.assertFalse(self.apply()["changed"])
        self.assertEqual(self.route.stat().st_mode & 0o777, 0o640)

    def test_unready_candidate_leaves_previous_route_untouched(self):
        self.new_ready = False
        previous = self.route.read_bytes()
        with self.assertRaisesRegex(RuntimeError, "timed out"):
            self.apply()
        self.assertEqual(self.route.read_bytes(), previous)

    def test_wrong_candidate_build_is_never_selected(self):
        with self.assertRaisesRegex(RuntimeError, "timed out"):
            rollout.switch(self.route, self.gateway, self.new, self.old, "c" * 40, timeout=0.25)
        self.assertEqual(self.active(), self.old)

    def test_failed_public_validation_restores_previous_route(self):
        self.gateway_failure = True
        with self.assertRaisesRegex(RuntimeError, "previous backend restored"):
            self.apply()
        self.assertEqual(self.active(), self.old)

    def test_unavailable_previous_backend_is_not_selected_for_rollback(self):
        self.gateway_failure = True
        self.old_ready = False
        with self.assertRaisesRegex(RuntimeError, "could not confirm rollback"):
            self.apply()
        self.assertEqual(self.active(), self.new)

    def test_interrupted_switch_resumes_from_durable_route(self):
        self.block_gateway = True
        child = subprocess.Popen(self.command(), stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            self.assertTrue(self.gateway_entered.wait(5), "switch did not reach public verification")
            child.kill()
            child.communicate(timeout=5)
            self.assertEqual(self.active(), self.new)
            self.gateway_release.set()
            result = subprocess.run(self.command(), capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertFalse(json.loads(result.stdout)["changed"])
        finally:
            if child.poll() is None:
                child.kill()
                child.communicate(timeout=5)

    def test_unknown_active_backend_is_preserved(self):
        rollout.selected(self.document)["url"] = "http://127.0.0.1:1"
        self.route.write_text(json.dumps(self.document))
        with self.assertRaisesRegex(ValueError, "third backend"):
            self.apply()
        self.assertEqual(self.active(), "http://127.0.0.1:1")

    def test_second_operator_cannot_switch_during_another_transaction(self):
        with self.route.with_suffix(".lock").open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            with self.assertRaises(BlockingIOError):
                self.apply()
        self.assertEqual(self.active(), self.old)

    def test_write_error_after_publish_rolls_back(self):
        original = rollout.replace
        calls = 0

        def interrupted_write(path, document):
            nonlocal calls
            original(path, document)
            calls += 1
            if calls == 1:
                raise OSError("directory sync failed")

        with patch.object(rollout, "replace", interrupted_write):
            with self.assertRaisesRegex(RuntimeError, "previous backend restored"):
                self.apply()
        self.assertEqual(self.active(), self.old)


if __name__ == "__main__":
    unittest.main()
