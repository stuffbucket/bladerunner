# Session State

## Decisions Made
- Controller interface for VM lifecycle (Ping, Stop, Status)
- WireFormat abstraction (LineFormat, JSONFormat) for protocol flexibility
- Transport abstraction (Unix, TCP) for connection flexibility
- Router pattern for command dispatch
- LocalController for server-side, Client for remote access

## Patterns Established
- Table-driven tests with format parameterization
- Mock types for isolated testing (mockConn, errorDialer)
- Constants for magic numbers (ports, timeouts, sizes)
- Explicit error ignoring with `_ =` for Close/Remove

## Invariants
- Context propagates through all handlers
- All enum switches have default cases (exhaustive)
- No nil error when err != nil (nilerr)
- No inline strings for status values (use constants)

## Technical Debt
- [ ] Transport interface lacks context parameter (noctx excluded)
- [ ] VM assets use exec.Command without context (noctx excluded)
- [ ] Test files exclude modernize linter (intentional)

## Coverage
| Package | Coverage |
|---------|----------|
| report | 98.1% |
| util | 88.9% |
| control | 80.5% |
| ssh | 73.5% |
| config | 69.1% |
| logging, provision, incus, vm, ui | 0% |

## Recent Commits
- `617ff1f` test(report): add tests for JSON/text report generation
- `e9df740` chore(lint): add mnd and modernize linters
- `9c46712` chore(lint): add linters for LLM code and architectural issues
- `813f271` chore(lint): add boundary-focused linters
- `9463d26` test(control): parametrize integration tests over wire formats
- `c3415c9` refactor(control): proper layer separation
