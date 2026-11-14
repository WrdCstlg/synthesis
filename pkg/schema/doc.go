// Package schema defines the wire contracts emitted and consumed by the
// synthesis public demonstration: configuration, history JSONL, external oracle
// stdin/stdout documents, tamper-evident lock status, and verdicts.
package schema

const (
	// ConfigVersion is the required value of synthesis.yaml `version`.
	ConfigVersion = "synthesis/v1"

	// VerdictSchema identifies a verdict.json document.
	VerdictSchema = "synthesis.verdict/v1"

	// OracleInputSchema identifies the JSON document written to an oracle's stdin.
	OracleInputSchema = "synthesis.oracle_input/v1"

	// OracleOutputSchema identifies the JSON document written to an oracle's stdout.
	OracleOutputSchema = "synthesis.oracle_output/v1"

	// OracleDefVersion identifies an external oracle YAML declaration.
	OracleDefVersion = "synthesis.oracle_def/v1"

	// ToolName is the CLI binary name.
	ToolName = "synthesis"
)
