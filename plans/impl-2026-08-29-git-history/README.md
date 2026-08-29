# Git History Tools Implementation Plan

> **Status:** DRAFT

## Overview

Adds three history MCP tools (`search_commits`, `get_diff`, `blame`) to muninn, backed by system git over the bare mirrors, plus the pinning/maintenance work the tools depend on. Spec: `plans/design-2026-08-29-git-history.md`.

**Problem:** Agents cannot answer history questions ("when/why did this change", "who wrote this line", "when was this string introduced") — Sourcebot, Sourcegraph, and GitHub's MCP servers all treat these as core, and zoekt indexes zero history.

**Goal:** Three parameter-shaped history tools with agent-safe defaults (pinned revs, first-parent, merge-base with warnings), hard output caps (file-boundary diff truncation), hermetic git invocations, and global subprocess timeouts — without breaking muninn's zero-idle, line-number-pinning guarantees.

Phased because the spec mandates the prerequisite (pinning completion + hermetic git plumbing) ship and be reviewable independently of the tools that consume it.

## Phases

| # | File | Delivers | Depends on | Review focus |
|---|------|----------|------------|--------------|
| 1 | `phase-1-plumbing.md` | Shared hermetic git runner with timeouts, git startup validation, two-generation pin refs, explicit gc after pinning, rev-error taxonomy | — | Subprocess hygiene, sync-concurrency safety, ref-transaction atomicity |
| 2 | `phase-2-tools.md` | `internal/githistory` package + three MCP tools with truncation/warning behavior | Phase 1 | Output-format contracts, truncation correctness, injection safety, tool descriptions |

## Phase Boundaries

- **1 → 2:** Phase 1 lands the shared `internal/gitcmd` runner (hermetic env, deadline, error classification) and the sync-side guarantees (two-gen pins, explicit gc) that make history-at-a-rev safe. Phase 2's tools are pure consumers; reviewing them is about output contracts, not git safety.
