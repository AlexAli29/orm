// Package diag holds the reconciliation report and its renderers.
//
// There is one structured [Report] and three ways to print it: [RenderText] for
// people, [RenderJSON] for programs, [RenderGitHub] for GitHub Actions
// annotations. The renderers read the same findings; none of them computes
// anything the others do not have.
//
// # Stability
//
// Finding codes are public API. E004 means one thing forever. Codes are never
// renumbered and never reused, so a code that is retired leaves a hole. The JSON
// field names are public API for the same reason.
//
// # Determinism
//
// Output is byte-identical across runs of the same inputs. Findings are sorted
// on a total order of stable keys, reports carry no timestamps, and positions
// are relative paths, so two machines checking the same commit produce the same
// bytes.
package diag
