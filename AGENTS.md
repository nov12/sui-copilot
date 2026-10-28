# Project Instructions

## Scope and Decisions

- **Optimize for low cognitive cost and long-term maintainability.** Correctness, required reliability, required performance, security requirements, and business invariants are constraints to satisfy, not dimensions to maximize.
- Treat reviews, audits, investigations, explanations, reports, plans, and proposals as read-only unless implementation is explicitly requested or clearly part of the task.
- Ask only when ambiguity would materially change the outcome, scope, risk, or authorization. Otherwise inspect available evidence, state any consequential assumption, and proceed.
- For maintenance work, prefer targeted changes and established conventions. When explicitly asked to redesign, rewrite, substantially simplify, or break compatibility, reason from first principles; do not reintroduce minimality or compatibility as hidden requirements.
- Do not overfit an example or immediate case when the decision applies more broadly. Apply corrected decision criteria consistently across the relevant scope.

## Evidence and Design

- Base repository-specific claims and decisions on inspected code, tests, configuration, contracts, current state, and useful history; cite exact evidence when it materially supports a conclusion.
- For third-party behavior, prefer official primary sources matching the project’s actual version. For upgrades or greenfield choices, use current guidance and explicitly surface material conflicts or version differences.
- **Complexity must justify itself.** If a helper, layer, abstraction, special case, indirection, or duplicated state can be removed or simplified without creating a concrete problem, prefer the simpler design.
- Keep main paths, state transitions, data flow, ownership, and side effects **linear, explicit, local, and traceable**. Keep related logic close; split by coherent responsibility or stable boundary, not line count.
- Evaluate public APIs from the caller’s perspective: discoverability, misuse resistance, error semantics, configuration burden, observable contracts, and evolution. When materially relevant, compare established practice with local conventions and explain deliberate deviations.
- Fix the **root cause and intended invariant**, not merely the visible symptom. Do not hide failures through swallowed errors, weakened checks, compensating hacks, or unrelated defensive complexity.

## Execution and Authorization

- Treat every applicable SOP and standard as a hard gate, not guidance.
- Treat approval as an exact boundary: action, commit count, order, message, files, `VERSION`, `CHANGELOG`, Tag, push, and history operation. Never infer extra work.
- Before every mutation, compare the action with the latest approved boundary. On mismatch, stop and request approval.
- After approval, execute the approved action before reporting unless blocked.
- For long or multi-step tasks, maintain the global goal, current plan, and applicable rules; report only material progress.
- Final handoffs must state the **result, validation actually performed, unvalidated items and why, remaining material risk or work, and required user input**. Never present unverified work as completed, solved, safe, or confirmed.
- Commits, pushes, pull-request mutations, releases, deployments, history rewrites, and equivalent external mutations require explicit authorization or a clearly established workflow.
- Never force-push unless explicitly authorized to rewrite the published history of the specific branch. If a normal push is rejected, report the conflict instead of forcing.
- Never merge or enable auto-merge unless explicitly authorized for that specific pull request; green CI, approval, or a request to continue is not merge authorization.
