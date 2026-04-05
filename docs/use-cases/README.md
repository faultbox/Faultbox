# Use Cases

Real-world scenarios showing how different engineering roles use Faultbox
from day one.

## Personas

| Role | Story | Key Value |
|------|-------|-----------|
| [Backend Engineer](backend-engineer.md) | Proves DB retry logic doesn't double-charge | Deepest value — WAL assertions, concurrency exploration |
| [QA Engineer](qa-engineer.md) | Tests 47 failure modes against existing containers | Fastest adopter — no code changes, just specs |
| [Mobile Engineer](mobile-engineer.md) | Discovers BFF response shapes under partial failure | Contract testing — verifies backend promises |

## Common Pattern

All three stories share the same arc:

1. **Start from a real incident** — double-charge, Black Friday outage, infinite loading
2. **Write a spec in minutes** — not days of infrastructure setup
3. **Test existing code** — no mocks, no code changes, real containers
4. **Get proof, not hope** — the assertion passes or shows exactly why it failed
5. **Add to CI** — never regress
