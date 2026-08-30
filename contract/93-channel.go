package contract

import "context"

// A Channel is a gated, backend-resolved handle to a space. warrant builds it
// (authorized, local-fs or remote-ssh transparently); a tool operates it through
// a kind-specific sub-interface. The base surface is the lifecycle face — Alive
// reports liveness, Close releases it. (Local file channels are stateless: Close
// is a no-op and there is no pool; pooling + Cut + idle-sweep belong to the
// stateful terminal channel, which the keeper recycles by ChannelKey.)
type Channel interface {
	Alive() bool
	Close() error
}

// ChannelKey identifies a channel for pooling + revocation: who (principal) +
// which space + which kind ("file" | "exec"). The keeper recycles by this key
// (the Acquire argument), not by any per-channel handle.
type ChannelKey struct {
	Identity string
	Space    string
	Kind     string
}

// FileEntry is one directory listing entry.
type FileEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

// FileChannel is DISCRETE, stateless file operations on a space (local fs now;
// sftp in slice 3). Each op is self-contained — no cwd/session state. Paths are
// space-relative; the impl refuses any path escaping the space root.
type FileChannel interface {
	Channel
	List(ctx context.Context, path string) ([]FileEntry, error)
	Read(ctx context.Context, path string) ([]byte, error)
	Write(ctx context.Context, path string, data []byte) error
	Mkdir(ctx context.Context, path string) error
	Remove(ctx context.Context, path string) error
	Rename(ctx context.Context, oldPath, newPath string) error
	// AbsPath resolves a space-relative path to a real local filesystem path (for
	// out-of-band delivery, e.g. deliver_file). A local backend returns the
	// validated absolute path (refusing any escape of the space root); a
	// remote/sftp backend returns an error — there is no local path to hand off.
	AbsPath(path string) (string, error)
}

// ExecResult is one command's outcome on an ExecChannel. Output is combined
// stdout+stderr (capped — Truncated marks it).
type ExecResult struct {
	Output    string
	ExitCode  int
	Truncated bool
}

// ExecChannel is a CONTINUOUS, stateful command session (a live local shell now;
// a remote ssh session in slice 3). cwd / env / shell variables persist across
// Run calls — it is one live shell, not a fresh subprocess per command. Run is
// serialized (one command at a time on the session). Being stateful, an exec
// channel is POOLED (open once, reused across Run calls, closed on idle / cut).
type ExecChannel interface {
	Channel
	Run(ctx context.Context, cmd string) (ExecResult, error)
}

// ChannelOpener vends identity-bound channels to a tool. The FLOW builds it
// (bound to warrant + the sender's principal + the default space); the turn core
// relays it to a tool via ToolContext. A tool asks for a space by name; space==""
// means the flow's default space. The tool never sees warrant or the identity —
// just "open a channel to space X" (already authorized for whoever is running).
type ChannelOpener interface {
	OpenFile(ctx context.Context, space string) (FileChannel, error)
	OpenExec(ctx context.Context, space string) (ExecChannel, error)
}

// CredentialOpener vends an identity-bound, kind-resolved credential client to a
// tool. The FLOW builds it (bound to warrant + the sender's principal); the turn
// core relays it via ToolContext. A tool asks for "the credential of kind K I'm
// authorized for" and gets a READY client (e.g. a *wordpress.Client) — never the
// secret, never warrant, never the credential name. Resolution is by-kind-unique:
// exactly one authorized credential of that kind → its built client; zero or many
// → an error the tool surfaces to the model (this is the http-credential sibling
// of ChannelOpener, which vends space channels; an http client is not a Channel).
type CredentialOpener interface {
	Build(ctx context.Context, kind string) (any, error)
}

// ChannelPool keeps stateful channels alive across calls. Provided by leaf/tools
// (the keeper); warrant Acquires through it when building a stateful channel
// (exec sessions, remote connections). Acquire returns the live channel for a
// key or builds + stores one; Recycle closes every channel for a principal
// (warrant Cut, on a permission change). Idle-sweep is internal to the pool.
// Stateless channels (local file) are NOT pooled — warrant builds them fresh.
type ChannelPool interface {
	Acquire(key ChannelKey, build func() (Channel, error)) (Channel, error)
	Recycle(identity string)
}
