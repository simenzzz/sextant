"""SQL parsing for the runtime's guard.

This package parses and reports; it never decides. The Go guard
(`internal/guard`) reads a `parse_summary.v1` and applies the policy — which
node kinds are allowed, which tables are in scope, whether a LIMIT is
acceptable. Splitting it this way puts the parsing where the good parser is
(sqlglot) and the rejection where the runtime is, and it keeps this service
free of any notion of what a "safe" query is.

See PLAN.md section 5.2 and the P1 planning decisions in that file.
"""

from src.sqlguard.parse import ParseRequest, summarize

__all__ = ["ParseRequest", "summarize"]
