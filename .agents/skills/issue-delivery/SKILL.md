---
name: issue-delivery
description: Assess or deliver a repository issue from specification readiness through minimal implementation, Draft PR, and review response while preserving human approval gates. Do not use for verification-only or standalone code-review requests.
---

# Issue Delivery

Use this skill when the user asks to assess an Issue's implementation readiness
or take it through implementation and review. It coordinates existing
repository and specialist skills; it does not grant permission to change the
Issue, publish changes, change PR state, or merge.

## Research and specification

Before implementation, inspect the issue and the relevant code, types, tests, documentation, CI configuration, and available history or PR context. Keep three things separate in working notes and reports:

- facts established from the repository or external records;
- inferences drawn from those facts;
- decisions that require the user's authority.

Do not ask the user for facts that can be discovered from available records or
tools. Investigate them directly or through the subagents required by the
repository's delegation rules. If the specification is ambiguous,
contradictory, or offers multiple materially different choices, use `grilling`
to expose every unresolved decision frontier and ask the user to choose. Do not
update the Issue or begin implementation until the user confirms the resulting
shared understanding.

After the user explicitly confirms the shared understanding reached through
`grilling`, use `domain-modeling` to reconcile the agreed vocabulary and
boundaries with the repository context. If that reconciliation exposes a new
ambiguity, contradiction, or material decision, return to `grilling` and obtain
the user's confirmation before documenting it or continuing. Capture
project-specific canonical terms and boundaries in the relevant `CONTEXT.md`
only when they materially help implementation, and record a durable design
decision in an ADR only when it is hard to reverse, surprising without context,
and the result of a real trade-off. For a trivial change, keep the model in the
working notes and avoid unnecessary CONTEXT/ADR churn. `domain-modeling`
records the agreement; it does not replace user approval or authorize an
Issue/specification mutation.

If an Issue or specification mutation is proposed, show the exact proposed
change and pause for explicit human approval before applying it. Updating an
Issue requires that approval even after the specification is agreed. Once any
approved mutation is applied (or when no mutation is needed), use `ponytail` to
evaluate the smallest coherent implementation path before declaring readiness.
Identify the existing mechanisms and tests to reuse, the minimum change
surface, and any unnecessary abstraction or scope expansion. This is design
analysis, not implementation authorization. Ponytail must not remove or weaken
a confirmed requirement; report scope-out improvements separately.

Implementation may start only when all of these are explicit and testable:

- contract and acceptance criteria;
- scope and non-goals;
- compatibility expectations;
- the relevant execution path and reuse opportunities have been inspected;
- a proportionate verification plan;
- any specification changes approved by the user;
- no unresolved decision that would change behavior, API, data, security, or scope.

Report this gate as `READY` or `NOT READY` with evidence. A readiness-only
request does not authorize implementation; proceed only if the user's current
request explicitly includes implementation.

## Implementation and verification

When implementation is authorized after the readiness gate, keep `ponytail`
active and implement the approved plan with the smallest coherent diff. Reuse
existing mechanisms, APIs, and tests where they already express the behavior.

Establish the worktree, branch, and base scope before editing, and preserve
unrelated or pre-existing user changes. Follow the repository's `AGENTS.md`,
including its TDD, delegation, and parent-agent diff-review requirements. Stop
if implementation reveals a specification contradiction, a new material
decision, or an expansion of scope/API/data/security.

Before treating implementation as verified, map each behavior-changing
acceptance criterion to a test that distinguishes the required behavior from
the prior behavior. For new behavior or regression tests, when practical,
confirm that they fail for the intended reason against the pre-change code or a
deliberately broken comparison, not merely that some test fails. For
asynchronous or concurrent behavior, establish prerequisites through observable
state rather than elapsed time alone, and ensure test resources are cleaned up
even when assertions or timeouts fail.

Before a commit or pull request, use the repository's `go-change-verifier`
skill when available. Do not ignore a failed or unavailable required check;
investigate, fix within scope, obtain equivalent evidence such as the required
CI job, or report the limitation or blocker. Commit, push, or create a Draft PR
only when the user has explicitly authorized that action. Ready-for-review and
merge are separate human decision points and require confirmation immediately
before changing either state. Keep the PR in Draft and include the Issue link,
scope summary, verification evidence, and any unavailable checks or limitations.

## Review handling

When the hosting integration is available, fetch the current inline review
threads as well as top-level review summaries. Otherwise, evaluate the review
material provided by the user and report that live thread state could not be
confirmed. For each human or automated comment, assess it against the current
diff, approved specification, and scope. Classify it as applicable,
inapplicable, or requiring a user decision; never apply a comment blindly. For
an applicable in-scope finding, make the smallest fix, keep it in an independent
review-follow-up commit when repository practice requires that, rerun relevant
verification, and reply with the evidence. Resolve only the corresponding
thread, and only after the fix and required CI checks succeed. Evaluate and
report whether another review adds value based on change size, residual risk,
and repository policy. Request one only when policy requires it or the user
approves; a trivial documentation or wording fix with green checks normally
does not need another review.

## Stop points

Pause and return control to the user at any of these boundaries:

| Boundary | Continue only after |
| --- | --- |
| Specification or Issue mutation | explicit human approval of the proposed change |
| A new material decision | the user selects or confirms the behavior |
| Scope, API, data, or security expansion | explicit authorization for the expansion |
| Required check failure, unavailable check, or contradiction | resolution, equivalent evidence, or human acknowledgement of the limitation |
| Optional review or re-review request | explicit human approval, unless repository policy makes it mandatory |
| Ready-for-review state | the human decides to promote the Draft PR |
| Merge | required checks pass and the human decides to merge after current review and unresolved-thread status are visible |

At every stop, report the evidence, the unresolved choice or failure, and the exact action awaiting approval. The skill does not turn a recommendation into authorization.
