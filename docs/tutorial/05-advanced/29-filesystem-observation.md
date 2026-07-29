# Chapter 29 — Watching the filesystem

You have made a service fail in every way the network allows. This chapter is
about a question none of that answers:

> **Which files did it touch?**

## The gap

Faultbox has always been able to tell you a service made 4,000 `write` calls.
It has never reliably been able to tell you *where*.

The reason is mundane. A syscall carries a **file descriptor** — the number
`7`, not the path `/var/lib/postgresql/data/base/1/1247`. Turning one into the
other means reading `/proc/<pid>/fd/7` from outside the process, after the
fact. That races the service, which may have closed and reused the descriptor,
and it truncates long paths.

So this class of question was simply unanswerable:

- Did this service write outside its data directory?
- Did the WAL get written before the commit was acknowledged?
- Did the config reload actually re-read the file, or serve a cached copy?
- Which files does a cold start touch that a warm one does not?

`watch()` answers them, by asking the sandbox instead of guessing from
outside.

## One-time setup

`watch()` needs gVisor to install a trace session when the sandbox boots, and
that setting lives in Docker's daemon configuration rather than per container.
So there is a one-time step:

```bash
sudo faultbox setup-trace
```

It prints exactly what it changed — and what it left alone:

```
  /etc/faultbox/trace.json         created
      trace session "Default", 4 points: openat, write, pwrite64, writev
  /etc/docker/daemon.json          updated
    + runtimes."faultbox-trace".path        = "/usr/local/bin/runsc"
      (left unchanged: runsc)
```

Then restart Docker, at a moment you choose:

```bash
sudo systemctl restart docker
```

Faultbox does not do this for you. The restart stops every container on the
machine, and that is not a decision a test run should make. Verify with
`faultbox setup-trace --check`.

## Your first watch

```python
determinism(runtime = "gvisor")

pg = service("pg",
    interface("sql", "postgres", 5432),
    image = "postgres:16-alpine",
    env = {"POSTGRES_PASSWORD": "faultbox"},
    healthcheck = tcp("localhost:5432", timeout = "60s"),
)

def workload():
    pg.sql.exec(sql = "CREATE TABLE IF NOT EXISTS t (id int)")
    pg.sql.exec(sql = "INSERT INTO t VALUES (1)")
    pg.sql.exec(sql = "CHECKPOINT")

def test_paths():
    watch(pg, files = ["/var/lib/postgresql/**"], run = workload)

    for e in events(where = lambda e: e.type == "file_io"):
        print(e.fields["op"], e.fields["path"])
```

Real output, not an illustration:

```
write /var/lib/postgresql/data/base/1/1247
write /var/lib/postgresql/data/base/1/1249
write /var/lib/postgresql/data/base/1/1259_fsm
...
```

Paths, not descriptors. That is the whole feature.

## The audit

The reason `watch()` exists is the **negative** assertion:

```python
def test_io_surface():
    watch(pg, files = ["/**"], ops = ["write"], run = workload)

    assert_never(lambda e: e.type == "file_io" and
                 not e.data["path"].startswith("/var/lib/postgresql"))
```

*This service never writes outside its data directory.* That catches a
dependency quietly writing to `/tmp`, a config loader reading a path nobody
documented, a credential file being touched that should not be.

## Why the run can fail for reasons that are not your spec

A negative assertion is only as strong as the trace behind it. "I never saw
it" and "it never happened" are different claims, and only one is what you
wrote.

So Faultbox **fails the test** rather than passing it when the observation was
incomplete:

| You will see | It means |
|---|---|
| `no sandbox ever connected to the trace sink` | Nothing was reported. Usually the service is not container-mode, or the host is not registered. |
| `dropped N trace point(s)` | The trace is a subset of what happened. The dropped operation could be the violation. |
| `received N trace point(s) that matched no launched service` | Observation ran and the trace was discarded. |

The drop case is the one you are most likely to meet. The sink starts losing
points somewhere between 17,000 and 47,000 per second, and a busy service can
exceed that. Narrow `files=` or `ops=`; both discard operations before they
ever become events.

## What it will not do

**It observes. It does not inject.** A trace point fires *after* the syscall
completed, so short writes, torn writes, `fsync` lies and "`ENOSPC` after N
bytes" are not available here. Those need a datapath that can change what the
service sees.

**There is no `fsync` point.** gVisor does not offer one, so `ops=["fsync"]`
is rejected at spec load rather than accepted and silently reporting nothing.
This matters more than it sounds: you can prove the WAL was **written**, but
not that it was **durable**. A test that claims otherwise is claiming more
than it measured.

**`read` and `close` are opt-in.** They roughly double trace traffic —
measured at 1,488 dropped points on a workload where the default set dropped
none — so enable them deliberately with `setup-trace --with-read` and expect
to narrow `files=` in exchange.
