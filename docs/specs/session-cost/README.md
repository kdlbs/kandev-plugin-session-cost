---
status: draft
system: session-cost
specification_version: 1
migration: complete
owners:
  - kdlbs
---

# Session Cost

## Purpose and ownership

The plugin owns session-cost lookup, report freshness, and presentation in the chat composer.
It resolves Kandev sessions through the Host API and calculates local transcript usage through tokscale.
The same owner defines backend behavior and desktop and phone outcomes.

## Boundaries

Kandev owns plugin execution, HTTP transport, authentication, and native UI primitives.
The plugin uses the SDK revision in `.kandev-sdk-ref` and keeps its existing API v1 manifest and read-only capability.
It does not own host request timeouts or tokscale's transcript format.

## Documentation

This repository previously documented behavior in its root README without requirement IDs.
The new documents define responsive lookup and recovery. The root README remains the operator and development guide.
Use the sibling Kandev catalog with `--root` pointing to this repository to find specifications.
