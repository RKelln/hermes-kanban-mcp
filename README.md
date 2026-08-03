# hermes-kanban-mcp

> **⚠️ EXPERIMENTAL — INCOMPLETE.** This repository is a design-phase experiment (2026-08): a pipeline test for design-to-ticket workflows, not production software. The implementation ticket set (T1-T8) is under review on the author's kanban board and nothing here has been implemented or validated beyond the design document. Expect breaking changes or abandonment. Use at your own risk; no support implied.

Go MCP server exposing the Hermes kanban board as MCP tools for remote opencode agents.

**Status:** design complete, ticket set in review on the hermes-agent kanban board (goal card `t_43a75b78`, tickets T1-T8 in triage). This directory is the canonical clone shell; implementation happens via worktrees per the ticket workspace discipline.

**Design:** `~/Documents/assistant/research/planning/kanban-opencode-workflow-design.md` (wiki design note).

**Stack:** Go (official `modelcontextprotocol/go-sdk` v1.7.0), streamable HTTP at `/mcp`, static bearer auth, single binary, systemd on experimance.
