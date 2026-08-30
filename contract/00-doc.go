// Package contract is the inter-module contract layer: the shared vocabulary
// that lets modules talk to each other WITHOUT importing each other. It holds
// capability interfaces and the data structures that flow across module
// boundaries — and nothing else. Zero behaviour. It is the bottom of the
// dependency graph: every module may import it; it imports no agentbob package
// at all (only the standard library).
//
// Membership rule — a type belongs here IFF it is part of a TRUNK-MEDIATED
// contract:
//
//  1. A capability interface that is registered on the trunk (i.e. a key in
//     the trunk capability registry — see package trunk). A consumer calls a
//     provider through this interface instead of importing the provider.
//
//  2. A data type that appears in such an interface's method signatures — the
//     payload that flows through trunk-mediated calls (the universal envelope:
//     message / attachment / agent-context / tool-call / chat-response, and the
//     like).
//
// Everything else stays out:
//
//   - A type used by exactly one module, not appearing in any contract
//     interface signature, stays in that module — even if it is "just a struct".
//   - A coupling that is DIRECT rather than trunk-mediated (e.g. a plugin and
//     its parent module: a source and the gateway) keeps its shared types in the
//     OWNING module; the other side imports them directly. Direct references do
//     not earn a place here.
//
// This package is deliberately a single flat package — the vocabulary is densely
// interconnected, and splitting it into sub-packages would invite import cycles.
//
// It is built from zero and grows as modules are ported or rebuilt: each module
// brings its contract types here when it lands (ported types verbatim, rebuilt
// modules' types defined fresh). It is never pre-populated speculatively.
package contract
