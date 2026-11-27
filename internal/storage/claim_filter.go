package storage

import (
	"fmt"
	"strings"
)

// WorkflowVersion names a workflow a worker runs and the source hash of the
// code it has for it. An empty SourceHash runs instances of any version.
type WorkflowVersion struct {
	Name       string
	SourceHash string
}

// versionFilter renders the claim condition "an instance of one of these
// workflows, started under a source hash this worker runs" and its
// arguments. As at resume, an instance without a hash, or a version without
// one, matches any. next renders the dialect's next placeholder.
func versionFilter(versions []WorkflowVersion, next func() string) (clause string, args []any) {
	parts := make([]string, 0, len(versions))
	args = make([]any, 0, 2*len(versions))
	for _, v := range versions {
		if v.SourceHash == "" {
			parts = append(parts, "workflow_name = "+next())
			args = append(args, v.Name)
			continue
		}
		parts = append(parts, fmt.Sprintf(
			"(workflow_name = %s AND (source_hash IS NULL OR source_hash = '' OR source_hash = %s))", next(), next()))
		args = append(args, v.Name, v.SourceHash)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// questionMarks renders "?" placeholders (SQLite, MySQL).
func questionMarks() string { return "?" }
