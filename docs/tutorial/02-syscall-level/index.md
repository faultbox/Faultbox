# Part 2: Syscall-Level Fault Injection

Break things at the kernel level — deny writes, delay connections, observe
internal behavior, and explore concurrency.

| Chapter | Duration | What you'll learn |
|---------|----------|------------------|
| [Fault Injection](03-fault-injection.md) | 25 min | deny(), delay(), scoped faults, named operations |
| [Traces & Assertions](04-traces.md) | 25 min | trace(), assert_eventually, assert_never, assert_before, ShiViz |
| [Concurrency](05-concurrency.md) | 25 min | parallel(), --explore=all, seed replay, nondet() |
| [Domain Model](06-domain-model.md) | 10 min | Name your operations; monitors and partitions get a deep dive in [Part 4](../04-safety/) |
