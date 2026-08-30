// Package migrate runs per-module schema migrations against a contract.DB.
//
// There is no central schema. Each module owns its ordered migration steps and
// calls Run in its Start (after it has Required contract.DB). This helper tracks
// the applied version PER MODULE in a single shared schema_version table and
// applies only the pending steps, so every module evolves its own tables
// independently. Cross-module ordering falls out of the trunk's Start order:
// each module migrates its own tables when it starts, after the modules it
// depends on.
package migrate
