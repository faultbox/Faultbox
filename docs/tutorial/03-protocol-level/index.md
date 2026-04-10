# Part 3: Protocol-Level Fault Injection

Move beyond syscalls — inject faults at the HTTP, database, and message
broker level. Return specific error codes, delay specific queries, drop
specific messages.

| Chapter | Duration | What you'll learn |
|---------|----------|------------------|
| [HTTP Protocol Faults](07-http-redis.md) | 25 min | fault(interface_ref), response(), delay(), drop() for HTTP protocol |
| [Database & Broker Faults](08-databases.md) | 25 min | Postgres query errors, Kafka message drops, gRPC status codes |
