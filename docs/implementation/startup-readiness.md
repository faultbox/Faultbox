# Startup readiness and fixed ports

Container `tcp()` checks targeting a declared local interface now use that
interface's protocol readiness probe. For example, Redis must answer `PING`
and MySQL must answer an authenticated `SELECT 1` before `seed` or dependent
services start. Docker accepting the TCP connection is insufficient. The
probe uses the same declared credentials and mapped host port as `ready()`.
An unrelated endpoint, a plain TCP interface, and non-container TCP checks
retain their explicit semantics. `ready()` continues to select the default
interface.

The `service_readiness_upgraded` trace event explains the upgrade with
`from`, `protocol`, `address`, and `reason` fields. It never includes credentials.
The readiness probe polls during the declared healthcheck timeout; it does not
restart a service or repeat a test.

`faultbox check` reports `PORT_IN_EPHEMERAL_RANGE` when a fixed local listener
falls in the execution host's ephemeral port range. It reads the actual Linux
`ip_local_port_range` or macOS sysctl values. Remote endpoints and Docker ports
published automatically are excluded. Run this check inside Lima for a suite
that executes inside Lima: the host and VM can have different port ranges.
The warning does not change the command's success exit status.

Startup distinguishes an already-bound fixed host port (`PORT_IN_USE`) from
a process that exited before readiness (`SERVICE_EXITED_BEFORE_READY`, with its
exit code and cause). Port preflight detects existing bindings but cannot reserve
a port for a subsequently launched process. It uses the actual mock bind address
and Docker's published bind address; native binary bind addresses are unspecified,
so their preflight probes only localhost (IPv4 and IPv6). This is best effort;
the actual process exit remains authoritative. Startup never guesses that every
crash is a port conflict or retries a failing service automatically.

Fixed port conflicts fail immediately at service preflight; the former blanket
ten-second wait before every test is removed. Remote and reused services never
participate in that check. Protocol readiness may poll during the configured
healthcheck timeout, but it does not restart services or republish inputs.

## Confirmed exit and cleanup isolation (G50)

A Linux thread-group leader may be a zombie while worker threads still hold
listeners. Native sessions therefore wait for a terminal `wait4` status for the
child. `WNOHANG` returning zero is still running, never exit code0. Reaping is
serialized with numeric-PID fallback signalling, and an unavailable exit status
is an explicit error rather than assumed success.

Managed binary services use their own PID namespace. Reaping its init confirms
namespace teardown, including detached descendants. Regression fixtures exercise
both a live worker with an exited leader and a `setsid` descendant holding a port.

After service/process and proxy shutdown, declared local fixed ports must become
bindable within the bounded cleanup phase. Remote services are excluded. A failed
check records `service_stop_error` with phase `ports_released`, fails the result
and prevents further tests from reusing that runtime. The check never kills a
process merely because it owns a port. A clean startup failure can still be
followed by another test once cleanup is confirmed; no automatic retry is added.

`PORT_IN_USE` is accompanied by a `port_in_use` event containing address/transport
and best-effort Linux listener owners. Owners carry PID, kernel start identity
and `comm` (short process name), never command arguments or environment. A service
and originating test are attached only when PID **and** start identity match a
process this runtime registered. Foreign/reparented owners may have no such
attribution. Restricted procfs, scan deadlines or unsupported platforms report
ownership as unavailable/incomplete rather than guessing. Ownership diagnostics
are separate from connection attribution and cannot authorize killing anything.
