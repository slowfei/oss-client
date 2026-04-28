# Provider driver rules
- Every provider lives in providers/<name>.
- Drivers must implement the common contract from pkg/uos.
- No provider-specific public types in exported SDK API.
- Map unsupported semantics to ErrUnsupported or documented capability gaps.
- Add/update contract tests for every new provider.
- Update docs/provider_matrix.md and examples when behavior changes.
