# Agent local state and create-only Docker adapter (candidate)

This Linux-only package has a persistent state kernel and a bounded local
`DockerPreparer.Prepare` / `Query` adapter. It can create an **unstarted** Docker
container and recover its original receipt. It never starts, execs, restarts,
activates or removes a workload. There is no service main, network control API,
user authentication, A08 wiring, SDK binding fence, or business execution path.
Activation and deployment as a full Agent remain disabled.

## Prepare/query contract

An authenticated control-plane integration must supply the existing instance
identity and accept its generation before calling this package. The adapter does
not enroll a host, invent a generation, or interpret local generation state as a
cross-host execution fence. It reuses `HostID`, `Requests`, `Resources` and
`MaxGeneration` in the original state file. `preparations` is an optional map in
that same journal; it is not a second resource owner or installation ledger.

The trusted service configuration defines an allowlist of profile names mapped
to an exact local `sha256:<image ID>`, a fixed absolute entrypoint and fixed args.
Requests can select a configured profile but cannot supply image tags, commands,
environment, ports, networks, mounts, devices or credentials. The adapter does
not pull images. A local image ID proves immutable local content, not the plugin
catalogue/signature/withdrawal trust chain; that chain remains an upstream gate.

`NewUnixDockerEngine` requires an explicit operator-selected local Unix socket.
It ignores `DOCKER_HOST` and registry credentials. Its only runtime actions are
image inspect, container inspect and container create through Docker Engine API
v1.44. Unsupported daemons return an error without falling back to a mutable tag
or another endpoint. The narrow versioned transport is based on the official
[Docker Engine API reference](https://docs.docker.com/reference/api/engine/).

The create policy fixes non-root UID/GID `65534:65534`, read-only rootfs, network
`none`, no added capabilities, `no-new-privileges`, no restart policy, disabled
healthcheck, no application environment other than a fixed PATH, and bounded
memory/CPU/PIDs. No volumes, port bindings, devices or secrets are supplied.
Images with inherited volumes, exposed ports, labels, non-PATH environment or
OnBuild instructions are rejected before intent/create. These deliberate image
restrictions require an explicitly reviewed compatible preparation image.

The existing OS file lock spans generation recheck, durable intent, one create
attempt, inspection and receipt persistence. The original instance/key
determines the container name; the request digest includes the host, generation
and immutable profile. Changed parameters conflict with the same key. A single
runtime resource is held by the host/instance across keys and generations, so
changing a key cannot create another container for an unresolved instance.
Different instances retain different names even for the same image/version/key.

A daemon request may finish after a client timeout; the local lock cannot cancel
it. `AcceptGeneration` therefore refuses a higher generation while the instance
has an intent or unknown preparation. A known prepared old container also retains
its exclusive runtime resource until a future approved cleanup/takeover flow.
No newer generation can reuse that resource through the generic state API.

- `prepared` means the exact original container exists in Docker's `created`
  state, has never started according to its inspected state, and matches the
  recorded ID, image, ownership labels and restricted configuration. `active`
  is always `false`. It is not an activation grant.
- `unknown` means the original intent cannot yet be confirmed. A timeout,
  process death, failed receipt write, missing container after intent, changed
  container or uncertain Docker response never causes another create attempt.
  Repeating Prepare on an existing intent only inspects the original name.
- `not_observed` means Query found no local committed intent for that identity.
  It does not establish global absence or authorize an unrelated new key.

Intent and the existing resource reservation are written together before create.
A successful inspection becomes a receipt only after the updated state is
persisted. If receipt persistence fails, the caller receives an error/unknown
result. A restarted process queries the original container instead of guessing
that the effect failed. Containers without the original local durable intent
are never adopted, including after local state loss.

Generic `Release` and `ReconcileRelease` refuse resources with a preparation
record. Forgetting a local reservation must not orphan or transfer a real
container. Container removal and stale-generation takeover require a future
explicit owner-reviewed recovery implementation; this adapter provides neither.
Once a preparation record exists, older kernels reject its new field rather
than silently discard it. Downgrade to an older state writer is unsupported.

## Persistent state assumptions

Explicit host enrollment must precede `Open`; a missing or corrupt state is
an error. The installer or control plane must independently authenticate the
host identity and resolve lost state rather than silently initialize it again.
Call `AcceptGeneration` before `Reserve`. Request identities persist after
release; a replay of a released request does not re-acquire its resource.
Older holds remain reserved after a generation change until reconciliation.

`ReconcileRelease` accepts an injected verifier but does not authenticate it.
Its two-phase local check prevents a stale local snapshot from releasing a
changed reservation; it cannot fence external writers or prove that a remote
instance has stopped. Integration requires a trusted verifier, external
execution fencing, and safe proof/ownership rules. Until then, do not wire
this package to an externally callable operation.

Resource identifiers are opaque strings and are not canonicalized. Adapters
must define and validate stable canonical identities (including port address
and protocol, hardware identity, and capacity semantics) before using them.
The state directory and all of its ancestors must be controlled by trusted
users. The code checks ownership and write permissions of the final directory,
and checks the final state and lock files for ownership, permissions, regular
file type and symlinks. It does not pin or validate ancestor directories, or
defend against replacement by another process with the same service UID.
Host identity anchored outside this file has not yet been implemented. Missing
local state requires reconciliation, not an
automatic reset. Persistence uses a same-directory temporary file, file sync,
rename and directory sync. Process death and original-key recovery are covered
by the tests below; power-loss and fsync-failure injection remain unverified.

The package has no database connection. Platform install records, installer
migration ledgers and business resources retain their existing owners. A local
Docker administrator or another process able to replace this service's state
or socket is outside this local fence. Even a prepared container cannot be
enabled for business until the real platform identity/current-binding and SDK
receiver fence are connected.

## Validation and remaining integration

Run the complete Linux module tests with:

```sh
go test -race -count=1 -v ./...
```

The tests exercise the actual state/adapter methods, same-key digest conflict,
distinct instances, current generation, restricted image/config inspection,
unknown outcomes, real state-file rename failures, two OS processes and SIGKILL
before/after a create side effect. The default Unix socket daemon is an explicit
HTTP fixture. It proves protocol and process recovery behavior, not real Docker
container creation. No environment restriction is silently converted to a pass.

The separate real Docker tests require a reviewed **already present** image and
an explicit local socket:

```sh
TP_AGENT_REAL_DOCKER=1 \
TP_AGENT_DOCKER_SOCKET=/var/run/docker.sock \
TP_AGENT_DOCKER_IMAGE_ID='sha256:<reviewed-local-image-id>' \
go test -race -count=1 -v -run '^TestPreparationRealDocker' ./...
```

They drop a real create response, reopen the original state, run two processes,
and kill a preparing process after the real daemon accepted create but before
receipt persistence. The test host identity is unique and starts with
`tp-takeover-b-installer-agent-`. Product container names remain deterministic
`tp-agent-prepared-<host/instance/key hash>`. Cleanup occurs only for each test's
original persisted intent after a complete ownership/config/state check, with
non-forced removal of a still-unstarted container. Failed ownership checks leave
the object for explicit review. Production code has no removal operation.

Real Docker tests skip explicitly without those variables. Their presence does
not imply execution in this candidate. Windows, a real authenticated Agent
service, connector activation/pause/upgrade/revoke, receiver-side business
fencing, full artifact trust, and J-G1 through J-G4 remain separate acceptance
work. This adapter must not be used to lift those guards.

## Activation, revocation and the service (added on `wp/i1-pg-profiles`)

`activate.go` adds the only path that starts a prepared container, and `service.go` /
`cmd/tp-agent` expose it. This closes the "no service main, no activation" gap, but it is **not**
the finished product: there is no enrolment of the host with the platform, no marketinstall/A08/SDK
wiring, and the bearer token is provisioned out of band.

* A prepared container stays `created` until a control plane with the token issues
  `/v1/grant` for the accepted generation (a lease of up to 24 h) and calls `/v1/activate`.
* Everything is re-checked under one lock: accepted generation, key, lease and revocation.
  An old generation (`409`), a revoked generation, an expired lease and an ungranted instance never start.
* `/v1/revoke` is durable before anything is stopped and is permanent for that generation.
* The sweeper (`-sweep-interval`) stops instances whose lease ended or whose generation was superseded.
* `/v1/retire` removes a stopped, superseded container (never forced) and releases the exclusive resource
  so the newer generation can prepare; a still-running or current generation is refused.
* A container that has ever started is never queried through the create-only adapter (that would
  rewrite its receipt as unknown); the activation ledger answers instead.
* API: POST JSON over the Unix socket (`0600`), `Authorization: Bearer <token>`:
  `/v1/accept-generation /v1/prepare /v1/grant /v1/activate /v1/revoke /v1/retire /v1/status /v1/sweep`.

Real-Docker evidence (opt-in, Linux): `TP_AGENT_REAL_DOCKER=1 TP_AGENT_DOCKER_IMAGE_ID=<id> TP_AGENT_DOCKER_SOCKET=<sock>`
for the package tests and `TP_AGENT_E2E_BIN` for `cmd/tp-agent`; the image needs a program at `/pluginstub`.

### Plugin profiles that need the platform (added later)

A trusted profile may carry three optional fields, never accepted from an API request: `network`
(an existing user-defined Docker network; `none`, `host`, `bridge`, `default` are refused), `env`
(fixed `KEY=VALUE` pairs; `PATH` cannot be overridden) and `reviewed_image_metadata` (accept the pinned
image's own exposed-port and environment declarations; volumes, labels and OnBuild stay forbidden and
nothing is ever published). All three default to off, so existing profiles and ledgers are unchanged.
The profiles file holds secrets and must be mode 0600. `tp-agentctl` posts one request to the socket
(`tp-agentctl -socket S -token-file F /v1/status instance=x generation=1`).
