# Go Code Style

Operational guide for developers and AI agents writing, modifying, or reviewing Go code. This document is **project-agnostic**: it defines defaults, not an architecture that every repository must adopt. Existing repository instructions, public contracts, and local conventions take precedence when they are deliberate and consistent.

## 1. Rule priority

Resolve conflicts in this order:

1. Correctness, security, and explicit product or API requirements.
2. Repository instructions (`AGENTS.md`, contributing guidelines, architecture decisions).
3. Existing code and conventions in the package being changed.
4. Established conventions across the module.
5. This guide.
6. General Go community conventions.

Do not turn an isolated anomaly into a convention. Avoid unrelated rewrites, dependency changes, and formatting churn. If this guide conflicts with an intentional local pattern, follow the local pattern and explain any meaningful trade-off.

## 2. General principles

- Prefer simple, explicit, understandable code over abstractions intended for hypothetical future use.
- Make illegal states difficult to represent; validate external inputs at system boundaries.
- Favor composition over inheritance-like embedding and concrete types over unnecessary interfaces.
- Keep packages cohesive, dependencies directional, and side effects visible.
- Use the standard library by default. Add dependencies only for a concrete need.
- Prefer immutable-by-convention values and narrow ownership of mutable state.
- Avoid global mutable state, hidden I/O, implicit initialization, and unnecessary background work.
- Use English for identifiers, comments, logs, and errors unless the project's requirements say otherwise.
- Write code for the version of Go declared by `go.mod`; do not assume newer language or standard-library features are available.

## 3. Project and package organization

- Follow the existing module layout. **Do not impose a directory structure on every Go project.**
- Packages should group related behavior, not merely types (`models`, `utils`, `helpers`, `interfaces` are usually poor default names).
- Use short, lowercase, descriptive package names without underscores or repetition where practical: `money`, `wallet`, `auth`, `storage`.
- Avoid package names such as `common`, `misc`, `base`, `types`, or `util` unless their purpose is genuinely clear.
- Prefer package-specific names: `wallet.Service` over `wallet.WalletService`, unless local conventions justify the suffix.
- Keep entrypoints such as `cmd/<app>/main.go` thin when a project actually has multiple commands or substantial wiring.
- Use `internal/` when Go's import restriction is useful. `pkg/` is optional, not a mandatory convention.
- Keep domain logic independent of HTTP routers, databases, queues, and framework-specific objects where practical.
- Define interfaces at meaningful dependency or testing boundaries, generally on the consuming side.
- Avoid circular imports and artificial splitting to satisfy a preferred diagram.
- Migrations, generated code, and integration fixtures belong in locations consistent with existing project tooling.

## 4. Formatting and imports

- Always apply `gofmt`; use `goimports` when available for import maintenance.
- Use tabs/formatting produced by `gofmt`, not custom alignment rules.
- Standard imports, third-party imports, and local module imports may be separated into groups when helpful; follow the repository's import tooling.
- Keep lines readable, but do not enforce a hard line-length limit that fights `gofmt`.
- Prefer early returns to deep nesting.
- Avoid semicolons and redundant parentheses.
- Keep functions focused; refactor based on cohesion and readability, not arbitrary line-count limits.
- Keep a change's diff focused; do not reformat unrelated files.

### Long function signatures

- Split long parameter lists across lines when a single-line signature is hard
  to scan. Apply this to constructors, functions, methods, and test helpers.
- Put the opening `(` after the function name, then one parameter per line.
- Write each parameter with its own type, including parameters that share a type.
- Include a trailing comma after the last parameter. Put the closing `)` and
  return types on the next line, followed by the opening `{`.
- Keep short, readable signatures on one line. Use `gofmt` for indentation.

```go
func New(
	store *postgres.Store,
	queue *queue.Client,
	service *application.Service,
	verifier *auth.Verifier,
	logger *slog.Logger,
	metrics *telemetry.Metrics,
) *Workers {
	return &Workers{
		store: store,
		queue: queue,
		service: service,
		auth: verifier,
		logger: logger,
		metrics: metrics,
	}
}
```

### Visual scanning and vertical spacing

- Make code easy to scan by separating logical blocks with one blank line.
- Separate consecutive guard clauses, loops, and the final return from adjacent
  logical blocks. Apply this inside nested blocks as well.
- Keep a call or assignment together with the `if err != nil` check that handles
  its result; place a blank line after the error-handling block.
- Separate setup from a following loop, and separate test setup, execution, and
  assertions when they represent distinct steps.
- Keep tightly related statements together. Do not insert blank lines between
  every statement, add consecutive blank lines, or pad the beginning/end of a block.
- Empty lines contain no spaces or tabs; finish with `gofmt`.

```go
func New(amount string, currency Currency) (Money, error) {
	if !validCurrency(currency) {
		return Money{}, ErrInvalidCurrency
	}

	if len(amount) > maxAmountLength {
		return Money{}, ErrAmountTooLong
	}

	units, err := parseMinorUnits(amount)
	if err != nil {
		return Money{}, err
	}

	return FromMinorUnits(units, currency)
}
```

The example illustrates spacing; `parseMinorUnits` is a placeholder, not a required abstraction.

## 5. Naming

### Packages, files, and identifiers

- Use `MixedCaps` or `mixedCaps` for identifiers, not snake_case; use conventional initialisms: `ID`, `URL`, `HTTP`, `JSON`, `API` (for example, `userID`, `httpClient`, `ParseURL`).
- Export only when another package needs the identifier. Prefer small, deliberate public APIs.
- Prefer concise, purpose-driven names: `user`, `order`, `ctx`, `err`, `tx`; use more descriptive names when the scope is larger or ambiguous.
- Avoid Java-style `GetX()` for plain accessors unless it avoids ambiguity or follows a required interface. Prefer `Balance()`, `Status()`.
- Boolean names should read naturally: `enabled`, `isReady`, `hasAccess` where appropriate; there is no mandatory prefix.
- Use `New` or `NewX` constructors when they improve clarity; constructors returning errors should validate invariants.
- Name methods after behavior: `Create`, `FindByID`, `Save`, `Delete`, `Publish`, `Close`, `Validate`.
- Name errors `ErrX` for exported sentinels, and error types `XError` when types are justified.
- Test files end in `_test.go`; file names are lowercase and descriptive, commonly snake_case for multiword filenames.
- Follow domain vocabulary consistently; avoid meaningless `Manager`, `Processor`, `Helper`, or `Impl` suffixes.

### Exported documentation

- Document exported declarations when their purpose or contracts are not already evident.
- Go doc comments for exported declarations should begin with the declaration name: `// Wallet ...`.
- Document ownership, concurrency guarantees, unusual constraints, units, nil behavior, retries, and side effects when relevant.
- Comments should explain **why** something is necessary, not narrate obvious code.

## 6. Data types and API design

- Use structs for related state and value objects; keep fields unexported when invariants must be enforced.
- Return concrete types when callers benefit from them. Return interfaces when a real abstraction or contract warrants it.
- Prefer explicit constructors for types requiring validation; use zero-value-friendly types when feasible.
- When the zero value is invalid by design, document it and validate before use.
- Use pointers when mutation, identity, shared ownership, large copying costs, or optionality require them; do not default to pointers for every struct.
- Keep value-receiver methods for logically immutable small values; use pointer receivers for mutation and generally for structs containing synchronization primitives.
- Do not copy values containing `sync.Mutex`, `sync.RWMutex`, or other non-copyable state after first use.
- Prefer `time.Time` and `time.Duration` rather than ambiguous raw timestamps or timeout integers. If wire protocols require numeric epochs, document the units.
- Use typed constants for closed, stable sets where useful; do not assume a named string type is automatically validated.
- Avoid `any` and reflection unless they solve a genuine generic requirement.
- Use generics when they remove meaningful duplication without obscuring domain behavior.
- Use `nil` consistently for absence where appropriate; distinguish missing from empty only when the API requires it.
- Do not add getters, setters, builders, or interfaces mechanically.

## 7. Functions and control flow

- Keep the happy path at the shallowest indentation level, using guard clauses for failures.
- Keep conditions focused on one rule. Split validations of unrelated concerns
  into separate guard clauses rather than one long `if` expression.
- Use `switch` for readable lists of allowed kinds or states. Name complex
  predicates when the name communicates a cohesive rule; avoid vague helpers.
- Preserve short-circuit safety when splitting checks for nil pointers, slice
  lengths, or parsing errors. In tests, check errors before inspecting results.
- Split long calls across lines, including calls inside error-checking `if`
  statements, instead of compressing their arguments onto one line.
- Return errors rather than panicking for expected or recoverable failures.
- Prefer a plain `for` loop when control flow, indexing, or side effects matter; use `range` where it makes intent clearer.
- Use `switch` when it improves readability over a long sequence of `if` checks.
- Limit function arguments by grouping genuinely cohesive values, not by creating arbitrary parameter objects.
- Avoid Boolean arguments that make a call ambiguous if separate functions or typed options would communicate intent better.
- Prefer direct calls to indirection through unnecessary helpers or interfaces.
- Use `defer` for cleanup close to resource acquisition; account for errors from writes, flushes, commits, and closes when they matter.
- Do not obscure control flow with clever one-liners.

## 8. Errors and failure behavior

- Check returned errors explicitly. Never discard them without a documented reason.
- Add useful context with `fmt.Errorf("load wallet %s: %w", id, err)` when propagating errors.
- Use `errors.Is` for sentinel matching and `errors.As` for typed errors; avoid error-string matching.
- Error messages should be lowercase, concise, and generally omit trailing punctuation; include the operation or subject.
- Preserve useful underlying errors with `%w`; avoid wrapping the same context repeatedly at every layer.
- Validate untrusted data at the boundary; keep domain validation close to the invariant being enforced.
- Differentiate expected business outcomes, validation failures, transient infrastructure failures, and unexpected internal failures.
- Avoid panics except for unrecoverable programmer errors or impossible states; do not use panic/recover for ordinary application flow.
- Do not expose internal errors, stack traces, sensitive identifiers, or secrets to API clients.
- Treat partial failure explicitly when coordinating more than one external system; do not claim atomicity that does not exist.

## 9. Context, I/O, and resource lifecycle

- For request-scoped operations that can cancel or time out, accept `context.Context` as the first argument, normally named `ctx`.
- Pass the caller's context to database, network, and other blocking operations that support it.
- Do not store request contexts in structs; pass them through method calls. Avoid `context.Background()` inside request work.
- Use context values only for request-scoped metadata, not optional function parameters or service dependencies.
- Define timeouts at appropriate boundaries rather than sprinkling arbitrary durations throughout business logic.
- Close responses, rows, streams, and connections owned by the current scope.
- Check `rows.Err()` after iteration, and treat transaction commit/rollback failures appropriately.
- Keep network and filesystem operations explicit and testable.
- Configure HTTP client/server timeouts, message size limits, and shutdown behavior as appropriate to the application.

## 10. Concurrency and state

- Start goroutines only when ownership, cancellation, termination, and error reporting are understood.
- Prefer synchronous code until concurrency offers a clear benefit.
- Never assume a map is safe for concurrent writes; protect shared mutable state with `sync` primitives or clearly owned goroutines/channels.
- Prefer channels for communication and coordination; prefer mutexes for guarding shared state when simpler.
- Avoid holding locks while performing network, database, or other unbounded I/O.
- Make bounded concurrency, queues, worker pools, retries, and backpressure explicit.
- Ensure goroutines stop on cancellation/shutdown; avoid leaks and unbounded fan-out.
- Use `errgroup` only when the added dependency or existing project utility is justified.
- Use atomics for simple, well-understood synchronization needs; do not build complex lock-free structures unnecessarily.
- Use `go test -race ./...` for tests covering concurrent behavior.
- Concurrency within a process does **not** imply correctness across multiple service instances: rely on storage-level constraints, transactions, and explicit distributed protocols where needed.

## 11. Persistence and distributed systems

*Apply this section only when the project has a database, cache, queue, or distributed operations.*

- Define transaction boundaries around invariants that must change atomically.
- Use parameterized queries and avoid concatenating untrusted input into SQL.
- Use database constraints as a final defense for uniqueness, foreign keys, and other durable invariants.
- For concurrent writes, use the appropriate locking, conditional updates, uniqueness constraints, or isolation semantics and verify affected rows.
- Make idempotency persistent when operations may be retried or redelivered across instances.
- Do not confuse in-memory locks, cache keys, or broker acknowledgements with durable atomicity.
- Use at-least-once delivery assumptions unless the actual system guarantees something stronger; consumers should be idempotent when replays are possible.
- Distinguish transactions within one database from distributed workflows spanning independent systems. Consider outbox/inbox patterns where appropriate.
- Keep external calls outside open transactions when feasible; document exceptions.
- Bound retry counts and use backoff/jitter when applicable. Retry only failures known to be safe to retry.
- Use structured migrations and never silently edit an already-applied production migration.
- Do not force a particular database driver, ORM, broker, cache, or schema naming strategy on every project.
- For financial quantities, avoid floating-point money calculations; choose exact integer minor units or decimal arithmetic according to currency scale and domain requirements.

## 12. HTTP and external boundaries

*Apply when relevant.*

- Separate transport concerns (routing, status codes, decoding/encoding) from domain behavior where it improves clarity.
- Validate request sizes, required fields, supported values, and formats explicitly.
- Use stable, documented request/response contracts and sensible HTTP status codes.
- Handle authentication and authorization at the appropriate boundary and enforce domain-level access checks where required.
- Set explicit content types; do not leak implementation details through client-facing error messages.
- Take care with JSON decoding: unknown fields, trailing data, duplicate keys, nullability, and integer precision may matter depending on contract strictness.
- Keep external API compatibility in mind; avoid accidental changes to field names and wire formats.
- Do not introduce a specific router, framework, DI container, or serialization package without project-specific justification.

## 13. Logging, metrics, and tracing

- Prefer the repository's existing logging API. For new standard-library-based projects, `log/slog` is a reasonable default.
- Keep log messages concise, actionable, and in English unless product requirements say otherwise.
- Include stable contextual fields (operation, request ID, resource ID), not secrets or unnecessary personal data.
- Avoid logging the same error at every layer. Log at boundaries where a failure is handled or reported.
- Choose levels by operational meaning: debug for diagnostic details, info for meaningful lifecycle events, warn for recoverable degradation, error for failed operations.
- Never log passwords, access tokens, credentials, full payment details, or confidential payloads.
- Add metrics/traces when observability needs justify them; do not invent telemetry infrastructure for simple packages.

## 14. Configuration and dependency wiring

- Read configuration at application startup or an explicitly defined boundary; do not scatter environment lookups throughout domain logic.
- Validate required settings early and report actionable configuration errors.
- Prefer explicit constructor injection over package globals and service locators for testable dependencies.
- Keep composition/wiring near the program's entrypoint.
- Do not hardcode secrets, environment-dependent URLs, or production credentials.
- Configuration formats (environment variables, flags, files) should follow project needs; none is universally mandatory.
- Do not mandate a dependency injection framework; plain Go constructors are often sufficient.

## 15. Tests

- Use the standard `testing` package by default; reuse existing helpers and assertion tools only if the repository already needs them.
- Name test functions `TestXxx`, benchmarks `BenchmarkXxx`, and examples `ExampleXxx`.
- Prefer table-driven tests when cases share the same behavior; descriptive case names should explain failures.
- Test public behavior, invariants, boundaries, and failure modes, not incidental implementation details.
- Cover zero values, invalid input, cancellation, concurrency, overflow, truncation, idempotency, and retries where relevant.
- Use `t.Helper()` in reusable test helpers; use `t.Cleanup()` for cleanup owned by the test.
- Avoid real network and production infrastructure in unit tests. Use integration tests for interactions with actual dependencies when valuable.
- Prefer small fakes or real lightweight implementations over deeply coupled mocking.
- Avoid `time.Sleep` as synchronization; use deterministic signals, deadlines, and controllable clocks where possible.
- Run `go test ./...`; use `go test -race ./...` for concurrency-sensitive changes, and `go vet ./...` when relevant.
- Do not add arbitrary tests to unrelated areas during a focused change.

Example:

```go
func TestParseRejectsInvalidInput(t *testing.T) {
    tests := []struct {
        name  string
        input string
    }{
        {name: "empty", input: ""},
        {name: "negative", input: "-1.00"},
        {name: "extra precision", input: "1.001"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            _, err := Parse(tt.input)
            if err == nil {
                t.Fatalf("Parse(%q) expected an error", tt.input)
            }
        })
    }
}
```

The example illustrates test structure; `Parse` is a placeholder for a function in the package being tested.

## 16. Security and correctness

- Treat input from HTTP, messaging, files, and databases as potentially invalid at trust boundaries.
- Use cryptographically secure randomness (`crypto/rand`) for tokens and secrets, never `math/rand`.
- Never rely solely on UI checks for authorization.
- Use constant-time comparisons where comparing secrets requires them; use vetted cryptographic primitives rather than inventing new ones.
- Avoid command injection, unsafe file paths, SQL injection, and unbounded resource consumption.
- Establish explicit maximum sizes for untrusted payloads and expensive operations.
- Handle arithmetic overflow and precision deliberately when correctness depends on numeric limits.
- Do not introduce `unsafe` without an explicit and compelling justification.

## 17. Anti-patterns

Avoid introducing any of the following without a demonstrated need:

- Interfaces with one implementation solely for aesthetics or mocking.
- Deep `service` → `manager` → `helper` → `repository` layers that do nothing but delegate.
- Framework-specific details in otherwise portable domain types.
- Global mutable registries, singleton services, or package-level hidden dependencies.
- Unbounded goroutines, retries, caches, or queues.
- Ignored errors, unexplained panics, and silent failure recovery.
- Excessive abstraction, reflection, or generics for trivial cases.
- Custom utility packages duplicating simple standard-library functionality.
- Tests that depend on order, timing luck, or live external systems without a clear integration-test purpose.
- Comments that restate the next line, TODOs with no useful context, and commented-out code left behind.
- Unrelated renaming, formatting, dependency upgrades, or architecture rewrites in a scoped task.

## 18. Workflow for AI agents

Before editing:

1. Read `go.mod`, relevant repo instructions, the target file, and nearby files/tests.
2. Identify package boundaries, exported API contracts, and local conventions.
3. Understand existing error handling, context propagation, synchronization, and persistence semantics.
4. Choose the smallest change that satisfies the request.

While editing:

1. Preserve public compatibility unless the task explicitly permits a breaking change.
2. Make dependencies and lifecycle explicit; keep changes local to the responsible package.
3. Follow Go idioms rather than translating Java, Kotlin, or TypeScript patterns mechanically.
4. Add focused tests for new behavior and important failure cases.
5. Write comments only for surprising decisions, contracts, and constraints.

Before finishing:

1. Run `gofmt` on changed Go files.
2. Run relevant tests and report what passed or could not run.
3. Run `go vet` and race tests when appropriate and feasible.
4. Check for accidental API changes, goroutine leaks, ignored errors, unsafe defaults, and sensitive logs.
5. Summarize the changes, remaining risks, and any unverified assumptions without claiming checks that were not executed.

## 19. Final principle

**Be idiomatic, predictable, and consistent.** The goal is not to force every Go project into one architecture. It is to help contributors and AI agents make narrowly scoped, maintainable changes while preserving the repository's contracts and established conventions.
