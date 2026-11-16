# Container upgrades behind Traefik

Use two ordinary Subrouter containers behind the existing Traefik listener.
Quadlet and systemd own container startup, healthchecks, restart policy and
shutdown. `switch-traefik.py` changes only the active backend and verifies the
serving build. It neither starts nor stops containers.

## Native health monitoring

Use one Quadlet template with separate image and loopback-port settings for
each instance. Keep each image pinned by digest. These shared settings make
startup wait for readiness and ongoing recovery depend on liveness:

```ini
[Container]
Notify=healthy
HealthStartupCmd=["/usr/local/bin/subrouter","probe","--ready"]
HealthStartupInterval=1s
HealthStartupRetries=0
HealthCmd=["/usr/local/bin/subrouter","probe"]
HealthInterval=10s
HealthRetries=3
HealthOnFailure=kill
StopTimeout=620

[Service]
Restart=on-failure
RestartSec=5
TimeoutStartSec=120
TimeoutStopSec=630
```

The startup probe uses `/_subrouter/ready`. After startup completes, the regular
probe uses `/_subrouter/health`. Later readiness loss does not kill a live
service. Podman schedules the healthchecks, and systemd handles recovery from
a process exit or sustained liveness failure. The JSON command form works
in the shell-free Subrouter image.

Set the worker's `--shutdown-timeout` below both outer stop timeouts. For
example, a 600-second worker deadline fits the values above. SIGTERM stops
accepting requests and waits for accepted HTTP and WebSocket requests within
that deadline. A connection still open when the deadline expires can be cut.

## Explicit promotion

A separate oneshot rollout unit can require the candidate instance and order
itself after that instance. With `Notify=healthy`, systemd runs the promotion
only after the candidate becomes healthy. A failed candidate start prevents
promotion. Starting or restarting a backend alone must not change routing.

Serialize releases in the deployment before changing an inactive instance.
The sequence is:

1. Start the candidate through systemd and wait for its healthy startup.
2. Run `switch-traefik.py` with its expected source commit.
3. After successful public verification, stop the previous unit through
   systemd and allow its configured shutdown grace.

Keep the routers and middleware in the existing managed configuration. Give
the release step a separate JSON-formatted `.yml` file containing the
`http.services.subrouter.loadBalancer.servers` list with one backend URL.
That file records the durable traffic selection. Mount its containing
directory into Traefik so atomic file replacement is visible to the watcher.
Persist the selected instance's boot configuration as part of activation.

The helper takes `--route`, `--gateway`, `--candidate`, `--previous` and the
full candidate `--revision`. Both backend URLs must be loopback URLs with
explicit ports. The gateway is the client-facing service URL. No credentials
are needed for its health and readiness probes.

An interrupted switch leaves its backend choice in the route file. Repeating
the same command verifies that choice without switching again. Failed public
verification restores the previous backend if it is ready. A failure never
means that the candidate is unused: retain both containers until routing has
been reconciled, because either may hold accepted connections. The previous
unit must remain available until promotion verification finishes.

## Verification

```sh
python3 deploy/containers/test_switch_traefik.py
go test ./cmd/subrouter -run 'TestProbe.*|TestShutdown.*'
```

These are local regression checks. The first live migration still needs an
explicit plan for the old worker's unshared admission limits, existing
connections and any local clients pinned to a particular backend port.
