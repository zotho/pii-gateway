# Security Policy

## Reporting a vulnerability

Please report security vulnerabilities privately via GitHub's
[private vulnerability reporting](https://github.com/PalenaAI/palena-litellm-pseudonymizer/security/advisories/new)
rather than opening a public issue.

We aim to acknowledge reports within 3 business days and to provide a remediation
timeline after triage. Please include:

- a description of the vulnerability and its impact,
- steps to reproduce or a proof of concept,
- affected versions or commit, and
- any suggested mitigation.

## Supported versions

Security fixes are provided for the latest released minor version. Until the
first stable (`1.0.0`) release, only the most recent `0.x` tag is supported.

## Handling of sensitive data

This service pseudonymizes personal data in transit. By design:

- Real ↔ pseudonym mappings are stored only in a session-scoped, short-lived
  Redis entry (default TTL 1 hour) and never written to logs.
- Structured logs and Prometheus metrics contain counts and entity-type labels
  only — never real names, pseudonyms, or the mapping. Session identifiers
  appear only as truncated hashes.

If you find a code path that could leak real names or mappings into logs,
metrics, error messages, or persistent storage, please treat it as a security
issue and report it privately.

## Supply chain

Release images are signed with Sigstore cosign (keyless, GitHub OIDC), ship
CycloneDX and SPDX SBOMs, and carry SLSA Level 3 provenance. Verify a release
image signature with the command in the corresponding GitHub Release notes.
