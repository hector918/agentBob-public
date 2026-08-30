// Package config is a thin foundation helper for module-owned configuration.
//
// There is no central god-config. Each module owns its own Config struct and
// (for heavy modules) its own yaml file; this package only provides the generic
// mechanism: load a yaml file over a defaults value. A module calls Load in its
// Start (and re-calls it on reload).
//
// Files carry overrides only: a missing file yields the defaults unchanged, and
// fields absent from a present file keep their default values. So with sane
// defaults in code, the on-disk config stays small regardless of how it is
// split across files.
package config
