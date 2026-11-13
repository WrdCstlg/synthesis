// Package buildinfo carries build provenance stamped into synthesis binaries
// via -ldflags -X at compile time.
package buildinfo

var (
	// Commit is the short git revision the binary was built from.
	Commit string
	// Dirty is "1" when the working tree had uncommitted changes at build time.
	Dirty string
	// SourceDate is Commit's committer timestamp in RFC3339 UTC.
	SourceDate string
)

// Stamped reports whether build provenance was embedded at link time.
func Stamped() bool { return Commit != "" }

// VerdictCommit returns the revision a verdict should record: the embedded
// commit stamp when present, otherwise fallback.
func VerdictCommit(fallback string) string {
	if Commit != "" {
		if Dirty == "1" {
			return Commit + "+dirty"
		}
		return Commit
	}
	return fallback
}
