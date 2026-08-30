package core

import "context"

// FileStorageBackend is the swappable execution backend behind the
// `file_storage` tool — a name-addressed file store ("space"). It mirrors
// the seven SFTP file-management verbs 1:1 and carries NO Run / exec verb:
// it is pure file manipulation, so the validation surface is the fixed set
// {verb, path} with no shell-injection face.
//
// Addressing: every verb takes a `name` (the space identifier). Same name →
// same store, so colleagues that pass the same name see each other's files.
// The backend maps the name to a storage area (the local backend roots it at
// <root>/spaces/<sanitized name>/; a future SSH backend maps it to a remote
// host). The tool shell owns name-default resolution + the permission gate;
// the backend only executes.
//
// Path arguments are space-relative for every verb except Get/Put, whose
// local side is an absolute, already path-validated bob-local path (the put/
// get bridge between the per-session sandbox and the space — see
// docs/spaces.md §1). Implementations MUST still confine space-side paths to
// the space root (no traversal escape).
//
// Concurrency: write verbs (Mkdir/Rmdir/Remove/Rename/Put) serialize per
// name; read verbs (List/Get) do not lock (docs/spaces.md §5).
type FileStorageBackend interface {
	// List returns the entries of directory `path` (space-relative) in space
	// `name`. A read verb — not locked.
	List(ctx context.Context, name, path string) ([]FileEntry, error)
	// Get copies space `name`'s file `src` (space-relative) out to `localDst`
	// (an absolute, already path-validated bob-local path). A read verb — not
	// locked.
	Get(ctx context.Context, name, src, localDst string) error
	// Put copies a bob-local file `localSrc` (absolute, already path-validated)
	// into space `name` at `dst` (space-relative). A write verb — locked per
	// name.
	Put(ctx context.Context, name, localSrc, dst string) error
	// Mkdir creates directory `path` (space-relative) in space `name`,
	// including any missing parents. A write verb — locked per name.
	Mkdir(ctx context.Context, name, path string) error
	// Rmdir removes the EMPTY directory `path` (space-relative) in space
	// `name`. A write verb — locked per name.
	Rmdir(ctx context.Context, name, path string) error
	// Remove removes the file `path` (space-relative) in space `name`. A write
	// verb — locked per name.
	Remove(ctx context.Context, name, path string) error
	// Rename moves/renames `from` to `to` WITHIN space `name` (both
	// space-relative). A write verb — locked per name.
	Rename(ctx context.Context, name, from, to string) error
}

// FileEntry is one directory entry returned by FileStorageBackend.List.
type FileEntry struct {
	Name  string // base name of the entry
	IsDir bool   // true for a directory
	Size  int64  // size in bytes (0 for directories)
	// ModTime is the entry's modification time as a Unix epoch (seconds).
	ModTime int64
}
