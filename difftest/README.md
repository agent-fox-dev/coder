# difftest — wire-level differential harness (NFR-TEST-06/07)

A separate Go module, on purpose. The reference bodies come from an
independent implementation — a vendor SDK at a pinned version, or recorded
live traffic — and whatever that costs in dependencies must not land in the
graph of anyone who imports AgentKit (REQ-GO-11).

```bash
cd difftest && go test ./...   # tests the harness itself
cd difftest && go run ./cmd/difftest
```

## This run is DARK, and that is the correct result

There are no scenarios, because there are no reference bodies. The harness
reports:

```
DARK: the run never reached the scenarios.
```

and exits **1**. That is NFR-TEST-07.3: *a run that never reached the
scenarios is dark, not passing. Zero compared scenarios is not a result.*

The gap is a corpus gap, not a code gap. NFR-TEST-06.3 forbids hand-authoring
a reference, and the reason is worth restating: a hand-authored expectation
encodes the same mental model as the code under test, so the two agree exactly
where both are wrong. Producing one needs a vendor SDK or a live API key, and
neither is available in this environment.

## Adding a scenario

```
scenarios/<name>/
  scenario.json                        # shared across every provider
  reference/anthropic-messages.json    # produced by the vendor SDK, never by hand
  reference/openai-completions.json
```

`scenario.json` is decoded with `DisallowUnknownFields` (NFR-TEST-06.6): a
misspelled option is a hard error, because otherwise both arms ignore it and
agree for the wrong reason.

Record the reference implementation, its version, and the exact command that
produced each file, per NFR-TEST-08.1 — `docs/PROVIDERS.md` (NFR-COMPAT-07) is
where that ledger belongs and it is not yet written.

## What is compared

Both arms capture through `OnPayload` (REQ-PROV-18), which stores the payload
and returns an error, so the harness aborts before the first byte and needs no
API key and no network.

Normalized, on purpose — hidden:

- object key order, except at a scenario's `order_sensitive_paths`
- string escaping
- whitespace

Not normalized, on purpose — a difference here **fails**:

- **number literal text.** `1024`, `1024.0` and `1e3` are one float64 and
  three different requests. Decoding with `UseNumber` and diffing the literal
  is what keeps them distinguishable.
- **array order, ever.** Message order and block order are the prompt.
- **key sets.**
- **`null` versus absent.** This is what makes REQ-PROV-16 enforceable:
  `omitempty` on a field whose zero value is meaningful produces *absent*
  where the reference produces an explicit value, and a comparison that treats
  the two as the same passes the exact bug the requirement exists to prevent.

Key order is moved to a side channel rather than discarded — `KeyOrderLines`
emits one line per object as `<path>\t<keys in original order>`, walking
objects by sorted key so both sides traverse identical paths.

## Exit codes (NFR-TEST-07)

| Code | Meaning |
|---|---|
| 0 | Every scenario `PASS` or `KNOWN`, and every ledger entry still fires |
| 1 | At least one `FAIL`, or a dark run |
| 3 | No `FAIL`, but at least one `FIXED` — a stale ledger entry |

A stale entry fails the run. It is a live, unattended permission slip: the day
someone reintroduces exactly that regression the harness reports `KNOWN` and
exits 0, and the defect ships with the harness's blessing. `FIXED` takes its
own code so "got worse" stays distinguishable from "got better, paperwork
behind" — both are non-zero because both need a human.

The summary always prints the `KNOWN` count and never renders as clean while
divergences are accepted.
