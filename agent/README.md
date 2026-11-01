# Agent local state kernel (draft)

This Linux-only package is an unconnected local state primitive. It does not
run workloads, expose an API, authenticate callers, contact the platform, or
constitute a deployable Agent. It must not be enabled as one.

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
The state directory and lock file must be private to the trusted Agent user;
file ownership, symlinks and a host identity anchored outside this file have
not yet been hardened. Missing local state requires reconciliation, not an
automatic reset. Persistence uses a same-directory temporary file, file sync,
rename and directory sync, but crash/power-loss and fsync-failure injection
have not been verified.

The package has no database connection. It is not a substitute for platform
install records, the installer migration ledger, or business owner APIs.
Current tests exercise local persistence, replay and contention only; they do
not prove real plugin installation, cross-host fencing or provider behavior.
