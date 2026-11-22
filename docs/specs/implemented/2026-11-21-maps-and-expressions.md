<!-- SPDX-License-Identifier: Apache-2.0 -->
# Spec: Maps and expressions (JSONata, CEL, templates)

Status: implemented (written as built). Date: 2026-11-21.

## TLDR

Three expression languages exist in the engine: JSONata for the `map` step (whole-payload
transform), conditions for `decision` and `route` (an `Evaluator`: a tiny `BasicEvaluator`, or
cel-go through `CELEvaluator`), and `{{operand}}` templates for `message` and `setProperties`. The
deployed runtime uses `BasicEvaluator`, which silently mis-reads expressions it does not support,
such as `a == 'x' && b == 'y'`. A profile-based map compiler (`maps.Compile`) exists but no step
uses it.

## Status and links

| | |
|---|---|
| Pull requests | https://github.com/nurtuo/nurtuo/pull/23 (squash commit `54ca076`) |
| ADRs | — |
| Issues | — |
| Code | `core/runtime/internal/engine/maps/` (`jsonata.go`, `funcs.go`, `compile.go`, `profile.go`, `script.go`), `core/runtime/internal/engine/eval.go`, `core/runtime/internal/engine/cel.go`, `MapRunner` in `core/runtime/internal/engine/steps.go` |
| User docs | [flow-reference.md](../../flow-reference.md#map), [flow-reference.md: decision](../../flow-reference.md#decision), [concepts.md](../../concepts.md) |

## Problem and goals

Flows reshape payloads (JSON to JSON), pick paths by document properties, and build small strings
(message bodies, property values). These must be expressible in the flow document without code,
evaluated per document, and safe to run on a shared runtime.

- Goals: JSONata transforms with a few helper functions; boolean conditions over `doc.id`,
  `doc.props`, `process.props`; string templates; compile once, evaluate many times.
- Non-goals (as built): XML or CSV in `map` steps (only the unused profile compiler reads XML);
  expression syntax checking at validation time; resource limits on evaluation.

## Design (as built)

**`map` step.** `MapRunner()` compiles `config.jsonata` once per expression (mutex-guarded cache)
with `maps.CompileTransform`, which uses `github.com/blues/jsonata-go v1.5.4` and registers
`customFuncs`: `$default(value, fallback)` (fallback for undefined, null or ""), `$padLeft(str,
width, pad)` and `$isEmpty(value)`. Per OK document (through `docs.Apply`): the payload is parsed
as JSON (an empty payload is `undefined` input), `Transform.Eval` runs, an undefined result
(`jsonata.ErrUndefined`) **drops** the document, otherwise the JSON-encoded result replaces the
payload. A compile error fails the step with `map_compile`; a non-JSON payload or evaluation error
fails only that document.

**Conditions.** `Evaluator.Condition(expr, doc, process)`:

- `BasicEvaluator` (`eval.go`, the default of `engine.New()` and therefore of the runtime):
  `true`, `false`, `!expr`, and one comparison `<operand> <op> <operand>` with the first operator
  found outside quotes among `== != <= >= < >`. Operands: quoted string, number, `true`/`false`,
  `doc.id`, `doc.props.NAME`, `process.props.NAME` (missing reads as ""). Comparison is numeric
  when both sides parse as floats, else lexical.
- `CELEvaluator` (`cel.go`): cel-go with variables `doc` (`id`, `props`) and `process` (`props`),
  programs cached per expression, output type must be bool or dyn. A missing property is an
  evaluation error (use `'k' in doc.props`).

**Templates.** `Evaluator.Template` (both evaluators use the Basic implementation) replaces each
`{{operand}}` with the operand value; an unterminated `{{` or unsupported operand is an error.
`message` sets the payload to the rendered template; `setProperties` renders each `value` and
stores it in the document (default) or process scope.

**Profile maps (unused).** `maps.Profile` (JSON or XML field trees with types, `Validate`,
`Parse`), `maps.Definition{Source, Target, Mappings[]{Target, Expr, When}}` and `maps.Compile`
(JSONata per field, CEL `when` over `src`) with `Map.Execute` / `ExecuteBytes`. Only tests call it.
`maps.ScriptStep` / `UnavailableScript` is a payload-only script hook; `engine.ScriptStepFor`
adapts a WASM module to it.

## Contracts

| Contract | Stability |
|---|---|
| `map.jsonata` semantics: payload in, JSON out, undefined drops | stable (documented in flow-reference) |
| Helper functions `$default`, `$padLeft`, `$isEmpty` | experimental |
| Condition grammar of `BasicEvaluator` | experimental; the schema says "CEL", the runtime accepts only this subset |
| CEL variables `doc.id`, `doc.props`, `process.props` | experimental (not selected by the runtime) |
| Template syntax `{{operand}}` | experimental |
| `maps.Profile`, `maps.Definition`, `maps.Compile` | internal, unused |

## Security

- JSONata and CEL run in-process with no sandbox, no context and no time or memory limit:
  `Transform.Eval` takes no `context.Context`, and `CELEvaluator` sets no cost limit.
- Templates and conditions only read document and process properties; they cannot call out.
- A map result is fully materialised (`json.Marshal`), then spooled; a 1 MiB HTTP body cap
  (listener default) bounds most inputs, queue messages are not bounded.

## Testing

| Test | What it covers | How to run | Notes |
|---|---|---|---|
| `TestMapRunner` (`engine/steps_test.go`) | JSONata transform through the interpreter | `cd core/runtime && GOWORK=off go test -race ./internal/engine/...` | no case for a non-JSON payload or an undefined result |
| `TestBasicEvaluator` (`engine/interp_test.go`) | Basic grammar and templates | same | no case for `&&`/`||`; a throwaway test on 2026-11-22 confirmed gap 1 (`false`, no error) |
| `TestCELEvaluator` (`engine/steps_test.go`) | CEL conditions, including `'k' in doc.props` | same | |
| `TestDecisionSplitsSet`, `TestRouteFirstMatchThenDefault`, `TestDecisionConditionErrorFailsDocument` | conditions inside the interpreter | same | |
| `TestParseJSON`, `TestParseXML`, `TestProfileValidate`, `TestMapExecute`, `TestMapToXML`, `TestCompileErrors`, `TestExecuteTypeMismatch`, `TestScriptStub` (`engine/maps/maps_test.go`) | profile compiler | same | covers code that no step uses |

## Gaps and risks

| # | Scenario (concrete) | Severity | Area | Mitigation / follow-up |
|---|---|---|---|---|
| 1 | Silent mis-evaluation: with `BasicEvaluator`, `doc.props.a == 'x' && doc.props.b == 'y'` splits at the first `==`; the right side `'x' && doc.props.b == 'y'` starts and ends with a quote, so it is read as one string literal. The condition is false for every document, no error is raised, and documents silently take the `false` or `default` path. | high | runtime evaluator | switch the runtime to `CELEvaluator`, or make `BasicEvaluator` reject `&&`, `||`, parentheses and multiple operators; add corpus-like tests |
| 2 | The runtime and the tests use different evaluators (`TestGetEmployeeFlowEndToEnd` sets `CELEvaluator`; `RunWith` uses `engine.New()`). Behaviour differs on missing props ("" versus error) and on any CEL beyond one comparison. | high | runtime wiring | one evaluator everywhere; see the [engine spec](2026-11-21-engine-interpreter.md) |
| 3 | A JSONata expression with a very large range or deep recursion over a large payload runs without a deadline or memory cap and can stall a worker or exhaust memory for the whole runtime; the run's context cancellation does not interrupt `Eval`. | medium | maps | run `Eval` with a timeout goroutine and payload size cap, or move heavy transforms to the WASM sandbox |
| 4 | `setProperties.value` is documented as CEL in the schema but rendered as a template: `doc.props.a + 'x'` is stored literally as that text. | medium | schema docs | rename to template semantics in the schema description or evaluate CEL |
| 5 | The profile-based map compiler (XML support, typed fields) is dead code from the flow's point of view; reviewers may assume XML mapping works. | low | maps | wire it to a step (for example a `mapping` component kind in bundles) or delete it |
| 6 | Expressions are compiled only when the first document arrives, so a JSONata syntax error is found at run time (`map_compile`), not at validation or deploy time. | low | validator, deployer | compile all expressions in `EngineDeployer.Apply` and fail the deployment |

## Improvement opportunities

- Validate CEL and JSONata syntax in the TS validator (or a deploy-time Go check) and return
  diagnostics with paths such as `/steps/2/config/cel`.
- Offer a CEL-based template (`{{ expr }}`) so the three languages become two.

## References

- Open Mercato: `workflows-wait-for-condition` (conditions evaluated over workflow state).

## Changelog

- 2026-11-22: created as built from `main` at `3d3d506`.
