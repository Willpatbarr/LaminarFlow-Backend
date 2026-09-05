# Architecture Decision Records

Numbered markdown files, one decision each, in [Nygard][nygard] shape: context, decision,
consequences. Numbers are assigned in order and never reused. A record is never edited to
change its decision — if a decision is reversed, write a new record and set the old one's
status to `Superseded by ADR-N`.

Same principle as `migrations/`: the file is a record of what was decided and why, not a
description of how the code currently looks. The code is the description of the code.

## What earns an ADR

A decision earns a record when someone reading the code later would reasonably ask "why is it
like this?" and the answer is not in the code:

* A constraint the language cannot express, so something else has to enforce it.
* A choice between two defensible designs, where the rejected one is what a reader would
  expect to find.
* A deliberate exception to a rule stated elsewhere in the codebase.
* An accepted limitation that looks like an oversight.

## What does not

* Anything the code already says. A record restating `Save` writes in one transaction is
  noise; the function says that.
* Product and architecture decisions that span both repos — those are Linear documents on
  `E-LAM-0000`. These records are about how *this* codebase is structured.
* Rules that live better next to the thing they govern. The expand-and-contract migration rule
  is in `migrations/README.md` because that is where you are standing when you need it.

## Records

| # | Decision | Status |
|---|----------|--------|
| [1](0001-write-path-enforcement.md) | Write-path enforcement for the document blob and the search index | Accepted |
| [2](0002-deployment-packaging.md) | Deployment packaging: one image, no shell, frontend embedded | Accepted |

[nygard]: https://github.com/joelparkerhenderson/architecture-decision-record/blob/main/locales/en/templates/decision-record-template-by-michael-nygard/index.md
