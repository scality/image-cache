<!--
Title: a conventional-commit subject scoped by component, in the imperative.
feat(agent): ..., fix(rpm): ..., ci: ..., docs: ...

First pull request here? CONTRIBUTING.md covers the toolchains and the
conventions, and .claude/REVIEW.md says what a review looks for.

Delete any section that does not apply rather than leaving it empty.
-->

## Component

<!-- agent, rpm, ci, docs. Several is fine. -->

## Problem

<!--
What is wrong or missing today, and who runs into it. Link the issue if there
is one. A reader who has never seen this code should understand why the change
exists before reading the diff.
-->

## Fix

<!--
What the change does, and the decisions a reviewer would otherwise have to
reconstruct from the diff. Say what you rejected and why when it is not
obvious.
-->

## Test

<!--
What you ran and what it showed: `make -C agent test`, `make -C agent lint`,
`make -C agent test-e2e`, `make -C rpm test EL=8` and `EL=9`. Say what you
could not run and why. A behaviour that can fail on a node deserves a test that
reproduces it.
-->

## Out of scope

<!--
What this deliberately leaves alone, and where it is tracked. A hazard you
found and chose not to fix belongs here rather than only in a comment.
-->

---

- [ ] The docs describing this behaviour are updated in the same pull request:
      `README.md`, `agent/README.md`, `DESIGN.md`, `agent/DESIGN.md`,
      `CONTRIBUTING.md`, whichever owns it.
- [ ] A change to the cache directory layout (subdirectory scheme, archive
      names, sentinel, permissions) lands in both halves and in
      `agent/DESIGN.md`. Not applicable to most pull requests.

Relates-to:
