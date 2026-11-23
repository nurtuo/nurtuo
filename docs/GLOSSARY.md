<!-- SPDX-License-Identifier: Apache-2.0 -->
# Nurtuo glossary

Canonical domain terms, kept sharp by the `domain-modeling` skill (format:
[GLOSSARY-FORMAT.md](../.agents/skills/domain-modeling/GLOSSARY-FORMAT.md)). Definitions only: no
implementation detail. Longer explanations are in [concepts.md](concepts.md); decisions are in
[docs/adr/](adr/).

## Language

**Flow**:
One integration process, stored as a `nurtuo.flow/v1` JSON document of steps and edges.

**Step**:
One node of a flow, of a given kind, with named output ports.

**Edge**:
A connection from one step's port to the next step.

**Document**:
The unit the engine processes: an id, properties, a payload and a status.
