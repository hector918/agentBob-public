// Package arch holds architecture-invariant guard tests: the sanctioned
// inter-module connection graph (every module's trunk Provides/Needs) and the
// import boundary (leaf/flow packages never reach across modules outside the
// trunk). They are deliberately strict — a change to either fails the build so a
// NEW module-to-module connection gets reviewed and approved on purpose, not by
// accident. See arch/10-boundary_test.go for the approval workflow.
package arch
