package compose

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"agentbob/contract"
)

// BoundChannels is the flow's identity-bound contract.ChannelOpener: it binds the
// turn's principal, its default space, and the SET of spaces this turn may open,
// then delegates to warrant. Tools call OpenFile without seeing warrant or the
// identity. nil warrant → channels unavailable.
//
// Allowed is the hard space gate: a non-empty space the model passes must be the
// default space or a member of Allowed, else OpenFile/OpenExec refuse it — so a
// turn can only reach the spaces its flow granted it (an agora member's own
// workspace + its companies'). Empty Allowed → only DefSpace is reachable (the
// normal single-user turn). The model-facing enumeration of these choices is
// stitched into the space-taking tools' descriptions by the flow; this is the
// enforcement behind it.
type BoundChannels struct {
	W        contract.Warrant
	Identity string
	DefSpace string
	Allowed  map[string]bool // space names this turn may open (besides DefSpace); nil → DefSpace only
}

// guard is the shared precondition for every face: a warrant must be present and the
// principal identified, else the corresponding sentinel error.
func (b BoundChannels) guard() error {
	if b.W == nil {
		return ErrNoWarrant
	}
	if strings.TrimSpace(b.Identity) == "" {
		return ErrUnidentified
	}
	return nil
}

func (b BoundChannels) OpenFile(ctx context.Context, space string) (contract.FileChannel, error) {
	if err := b.guard(); err != nil {
		return nil, err
	}
	s, err := b.resolveSpace(space)
	if err != nil {
		return nil, err
	}
	return b.W.File(ctx, b.Identity, s)
}

func (b BoundChannels) OpenExec(ctx context.Context, space string) (contract.ExecChannel, error) {
	if err := b.guard(); err != nil {
		return nil, err
	}
	s, err := b.resolveSpace(space)
	if err != nil {
		return nil, err
	}
	return b.W.Exec(ctx, b.Identity, s)
}

// Build resolves the identity-bound, kind-unique credential client (the
// CredentialOpener face). The tool asks by kind ("wordpress"); warrant gates +
// builds it. Same identity guards as the channel openers — no warrant or no
// principal → refused before reaching warrant.
func (b BoundChannels) Build(ctx context.Context, kind string) (any, error) {
	if err := b.guard(); err != nil {
		return nil, err
	}
	// outer-bob supply: warrant projects its own matrix for the principal, then judges.
	return b.W.ResolveCredential(ctx, b.W.Grants(ctx, b.Identity), kind)
}

// resolveSpace maps the model's space argument to the space to open: empty → the
// default; otherwise it must be the default or in Allowed, else a refusal that
// lists the reachable spaces (so the model can self-correct).
func (b BoundChannels) resolveSpace(space string) (string, error) {
	space = strings.TrimSpace(space)
	if space == "" || space == b.DefSpace {
		return b.DefSpace, nil
	}
	if b.Allowed[space] {
		return space, nil
	}
	return "", fmt.Errorf("space %q is not available to you; reachable spaces: %s", space, b.reachableList())
}

// reachableList renders the sorted set of reachable space names for an error hint.
func (b BoundChannels) reachableList() string {
	names := make([]string, 0, len(b.Allowed)+1)
	if b.DefSpace != "" {
		names = append(names, b.DefSpace+"（默认）")
	}
	for s := range b.Allowed {
		names = append(names, s)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "（无）"
	}
	return strings.Join(names, "、")
}

// SpaceSet builds the BoundChannels.Allowed lookup from a list of space names
// (nil/empty → nil, meaning DefSpace-only). A flow uses it to load the turn's
// reachable spaces (e.g. an agora member's company spaces).
func SpaceSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	m := make(map[string]bool, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			m[n] = true
		}
	}
	return m
}

// ErrNoWarrant is returned by BoundChannels when no warrant is wired.
var ErrNoWarrant = errors.New("no warrant wired — channels unavailable")

// ErrUnidentified is returned when a turn with an EMPTY principal tries to open a
// capability channel (D41 defense-in-depth). A locked-down agora turn (inbox unresolved
// → at.Principal="") builds BoundChannels{Identity:""}; today it's safe only because its
// tool bag is empty, but the local File/Exec channels confine by spaceDir and ignore
// identity, so an unidentified turn must be refused HERE rather than relying on "no tool
// happens to exist" — capability without a principal is never granted.
var ErrUnidentified = errors.New("turn has no identity — capability channels unavailable")

var (
	_ contract.ChannelOpener    = BoundChannels{}
	_ contract.CredentialOpener = BoundChannels{}
)
