#!/usr/bin/env python3
"""Switch a Traefik service between two already-started Subrouter containers."""
import argparse
import fcntl
import json
import os
from pathlib import Path
import re
import stat
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def backend_url(value):
    url = urllib.parse.urlsplit(value)
    if (url.scheme not in ("http", "https") or url.hostname not in ("127.0.0.1", "::1")
            or not url.port or url.username or url.password or url.query or url.fragment
            or url.path not in ("", "/")):
        raise ValueError("backend URLs must name a loopback address and explicit port")
    return value.rstrip("/")


def selected(document):
    servers = document["http"]["services"]["subrouter"]["loadBalancer"]["servers"]
    if len(servers) != 1 or set(servers[0]) != {"url"}:
        raise ValueError("service subrouter must have exactly one backend URL")
    return servers[0]


def replace(path, document):
    fd, temporary = tempfile.mkstemp(dir=path.parent, prefix=".subrouter-", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as output:
            os.fchmod(output.fileno(), stat.S_IMODE(path.stat().st_mode))
            json.dump(document, output)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        Path(temporary).unlink(missing_ok=True)


def get(url, path, timeout):
    # A local backend probe must not be sent through an ambient HTTP proxy.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(url.rstrip("/") + path, timeout=min(2, timeout)) as response:
        return json.load(response)


def revision(url, timeout):
    return get(url, "/_subrouter/health", timeout)["build"]["commit"]


def wait(check, timeout):
    deadline, last = time.monotonic() + timeout, "unexpected response"
    while time.monotonic() < deadline:
        try:
            result = check(max(0.01, deadline - time.monotonic()))
            if result:
                return result
        except (OSError, ValueError, KeyError) as error:
            last = str(error)
        time.sleep(min(0.1, max(0, deadline - time.monotonic())))
    raise RuntimeError(f"verification timed out: {last}")


def switch(route, gateway, candidate, previous, expected, timeout=15):
    candidate, previous = backend_url(candidate), backend_url(previous)
    if candidate == previous or not re.fullmatch(r"[0-9a-f]{40}", expected):
        raise ValueError("provide distinct backend URLs and a full source commit")
    route = Path(route)
    with route.with_suffix(".lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        document = json.loads(route.read_text())
        server = selected(document)
        current = server["url"]
        if current not in (previous, candidate):
            raise ValueError("route selects a third backend; refusing to overwrite it")
        candidate_selected = current == candidate
        try:
            wait(lambda remaining: get(candidate, "/_subrouter/ready", remaining).get("ok")
                 and revision(candidate, remaining) == expected[:12], timeout)
            if not candidate_selected:
                server["url"] = candidate
                candidate_selected = True
                replace(route, document)
            wait(lambda remaining: revision(gateway, remaining) == expected[:12], timeout)
        except Exception as error:
            if candidate_selected:
                # The candidate may already have accepted connections. Only
                # restore routing here; neither process is stopped.
                try:
                    wait(lambda remaining: get(previous, "/_subrouter/ready", remaining).get("ok"), timeout)
                    old_revision = revision(previous, timeout)
                    latest = json.loads(route.read_text())
                    if selected(latest)["url"] not in (candidate, previous):
                        raise ValueError("route changed during verification")
                    selected(latest)["url"] = previous
                    replace(route, latest)
                    wait(lambda remaining: revision(gateway, remaining) == old_revision, timeout)
                except Exception as rollback:
                    raise RuntimeError(f"{error}; could not confirm rollback: {rollback}; keep both containers") from error
                raise RuntimeError(f"{error}; previous backend restored; keep both containers") from error
            raise
        return {"active": candidate, "previous": previous, "revision": expected,
                "changed": current != candidate}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--route", required=True, help="existing JSON-formatted Traefik service file")
    parser.add_argument("--gateway", required=True, help="client-facing Subrouter base URL")
    parser.add_argument("--candidate", required=True, help="private candidate URL")
    parser.add_argument("--previous", required=True, help="private previous URL, also used when retrying")
    parser.add_argument("--revision", required=True, help="candidate's full source commit")
    parser.add_argument("--timeout", type=float, default=15, help="verification timeout in seconds")
    args = parser.parse_args()
    if args.timeout <= 0:
        parser.error("timeout must be positive")
    try:
        result = switch(args.route, args.gateway, args.candidate, args.previous, args.revision, args.timeout)
    except (OSError, RuntimeError, ValueError, KeyError) as error:
        parser.exit(1, f"switch failed: {error}\n")
    print(json.dumps(result))


if __name__ == "__main__":
    main()
