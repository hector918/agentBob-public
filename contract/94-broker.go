package contract

import "context"

// Broker is warrant's secret backend: it builds a configured client for a named
// credential (an ssh client now; http/db later) from the on-disk vault WITHOUT
// the secret reaching the caller — warrant gets a ready client, never the key.
// Build returns `any` so the contract stays free of provider types (the caller
// type-asserts: *ssh.Client, …). Provided by leaf/credentials; consumed by
// warrant (which gates credential:use:<name> BEFORE calling Build).
type Broker interface {
	Build(ctx context.Context, name string) (any, error)
	// NamesByKind lists the vault credential names whose kind= field equals kind
	// (e.g. all "wordpress" credentials). warrant uses it to resolve the unique
	// credential of a kind a GrantSet admits (warrant.ResolveCredential); it reads
	// only the kind field, never a secret. Order is unspecified; a malformed vault
	// file is skipped, not an error.
	NamesByKind(ctx context.Context, kind string) ([]string, error)
}
