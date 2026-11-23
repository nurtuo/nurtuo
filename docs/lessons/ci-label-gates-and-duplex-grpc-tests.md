# Lessons: label-gated CI steps and full-duplex gRPC tests

## A CI label only works if the workflow re-runs on label changes

`pull_request` triggers only on `opened`, `synchronize` and `reopened` by default. Adding the
`buf skip breaking` label afterwards started no run, and the `proto` job kept failing on the
deliberate break. Fix: add `labeled` and `unlabeled` to `on.pull_request.types`, and gate the step on
`contains(github.event.pull_request.labels.*.name, 'buf skip breaking')`. The label list comes from the
event payload, so it reflects the labels at the moment the run was triggered.

## A bidirectional-streaming test must read while it writes

`Large_payload_is_chunked_with_bounded_chunks` wrote the whole 1 MiB request stream before reading any
response. The echo connector streams its output while it is still reading input. Once the HTTP/2
flow-control windows filled in both directions, each side waited for the other, the test hung for
about 4m40s and then failed with "Connection reset by peer". Small payloads never showed it. The job had been
non-blocking and then failed to compile (CS0542), so the test had not run in CI before. Fix: start
draining the response stream before writing the request.
