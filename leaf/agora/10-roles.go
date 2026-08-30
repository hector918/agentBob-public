// Package agora is the in-process mirror of the org graph persisted by
// leaf/agora/store. OrgCache holds the entity rows + each Company's parsed
// permissions_yaml so the hot paths (identity resolve / send_message / tool
// projection) read RAM, never pg. Writes go store-FIRST then cache
// (write-through) so a failed store write never desyncs the mirror.
//
// This package imports leaf/agora/store (it holds store.Company etc.) per
// docs/agora-port.md §4 boundary A — the heavy entities are the module's own
// data and never cross the trunk.
package agora

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// RoleDef is one entry from a Company's permissions_yaml: the grant-string list
// for a role. Format is strict 3-segment "kind:verb:name" (e.g.
// "tool:use:read_file"). An empty Grants list means the role authorises nothing
// (fail-closed). Wildcards are not supported.
type RoleDef struct {
	Grants []string
}

// RolesConfig is a Company's parsed permissions_yaml. Empty (no body / empty
// body) is valid — a role lookup miss contributes no grants and falls through
// to fallback-deny. Roles is always non-nil after parse.
//
// The on-disk shape is the per-grant matrix admin reads at a glance (same as
// $BOB_HOME/permissions.yaml): a `roles:` list of role names + a `grants:` map
// of capability → role → on/off. UnmarshalYAML flattens that into the
// per-role Grants []string the cache consumers expect.
type RolesConfig struct {
	Roles map[string]RoleDef
}

// matrixFile is the on-disk yaml shape decoded before flattening:
//
//	roles:  [- name, ...]
//	grants: {kind:verb:name: {role: on|off, ...}, ...}
type matrixFile struct {
	Roles  []string                     `yaml:"roles"`
	Grants map[string]map[string]string `yaml:"grants"`
}

// UnmarshalYAML projects the matrix yaml into the flat RolesConfig. Every check
// is LOUD (returns an error rather than silently coercing) so an admin hand-edit
// typo surfaces instead of silently dropping or widening a grant — ported from
// skeleton's permissions.DecodeMatrix (trunk has no shared permissions package,
// so the validation lives here, agora's only consumer):
//   - role name matches roleNameRe (lowercase, 1-64) and is declared exactly once
//   - grant name is a strict 3-segment kind:verb:name (no wildcards)
//   - every role referenced by a grant cell is declared under `roles:`
//   - a cell state is on/off (any other value errors — NOT coerced to off)
//
// Grant iteration is sorted so a role's Grants order is deterministic across
// reloads. Empty body → empty Roles map (valid; a role lookup miss → fail-closed).
func (rc *RolesConfig) UnmarshalYAML(node *yaml.Node) error {
	var mf matrixFile
	if err := node.Decode(&mf); err != nil {
		return err
	}
	if len(mf.Roles) == 0 && len(mf.Grants) == 0 {
		rc.Roles = map[string]RoleDef{}
		return nil
	}
	declared := make(map[string]bool, len(mf.Roles))
	out := make(map[string]RoleDef, len(mf.Roles))
	for _, r := range mf.Roles {
		if err := validateRoleName(r); err != nil {
			return err
		}
		if declared[r] {
			return fmt.Errorf("agora roles: role %q declared twice in `roles:` list", r)
		}
		declared[r] = true
		out[r] = RoleDef{}
	}
	// Sorted so each role's Grants slice is deterministic across reloads.
	names := make([]string, 0, len(mf.Grants))
	for name := range mf.Grants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateGrantName(name); err != nil {
			return err
		}
		if err := rejectRawArrangementGrant(name); err != nil {
			return err
		}
		for role, state := range mf.Grants[name] {
			if !declared[role] {
				return fmt.Errorf("agora roles: grant %q references role %q not in `roles:` list", name, role)
			}
			switch normaliseState(state) {
			case "on":
				rd := out[role]
				rd.Grants = append(rd.Grants, name)
				rd.Grants = append(rd.Grants, expandArrangementBundle(name)...) // coarse role-bundle → the underlying tool grants
				out[role] = rd
			case "off":
				// disabled — present in the file for visibility, no grant.
			default:
				return fmt.Errorf("agora roles: grant %q role %q state %q is not one of [on, off]", name, role, state)
			}
		}
	}
	rc.Roles = out
	return nil
}

// expandArrangementBundle maps a coarse arrangement role-bundle grant to the underlying
// tool grants. The bundles are the ONLY way to grant arrangement tools in permissions_yaml
// (the four raw tool grants are rejected — see rejectRawArrangementGrant), so a half-grant
// can't be expressed. The tools themselves still exist; the bundle expands to them here so
// AuthorizeTools / roleCanProcess (which check tool:use:arrangement_*) work unchanged. nil
// for non-bundle names.
//
//	arrangement:role:creator      → arrangement_define + arrangement_inject
//	arrangement:role:collaborator → arrangement_pull   + arrangement_submit
//
// See docs/arrangement.md §12.
func expandArrangementBundle(name string) []string {
	switch name {
	case "arrangement:role:creator":
		return []string{"tool:use:arrangement_define", "tool:use:arrangement_inject"}
	case "arrangement:role:collaborator":
		return []string{"tool:use:arrangement_pull", "tool:use:arrangement_submit"}
	}
	return nil
}

// rejectRawArrangementGrant refuses the four individual arrangement tool grants in
// permissions_yaml — arrangement tools are granted ONLY via the bundles (creator /
// collaborator), so no one can express a half-grant or scatter the four. Error points at the
// bundle. (The tools still exist + are reached via expandArrangementBundle.)
func rejectRawArrangementGrant(name string) error {
	switch name {
	case "tool:use:arrangement_define", "tool:use:arrangement_inject",
		"tool:use:arrangement_pull", "tool:use:arrangement_submit":
		return fmt.Errorf("agora roles: grant %q is not granted directly — use arrangement:role:creator (define+inject) / arrangement:role:collaborator (pull+submit)", name)
	}
	return nil
}

// normaliseState canonicalises an admin-typed cell value; "" for anything
// unrecognised so the caller hard-errors instead of silently coercing to off
// (the "silent typo coerced to off" bug class).
func normaliseState(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes":
		return "on"
	case "off", "false", "no":
		return "off"
	default:
		return ""
	}
}

var (
	// roleNameRe: lowercase, first char [a-z], rest [a-z0-9_-] — so a role name is
	// safe in yaml keys / slash / logs / SQL without escaping or case surprises.
	roleNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	// grantRe: strict 3-segment kind:verb:name. kind/verb are bob vocab (lowercase);
	// the name segment comes from registries that allow a leading digit + uppercase.
	// Wildcards (`*`) are rejected — the matrix refactor retired them.
	grantRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*:[a-z][a-z0-9_-]*:[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
)

func validateRoleName(name string) error {
	if name == "" {
		return fmt.Errorf("agora roles: role name is empty")
	}
	if len(name) > 64 {
		return fmt.Errorf("agora roles: role name %q too long (max 64)", name)
	}
	if !roleNameRe.MatchString(name) {
		return fmt.Errorf("agora roles: role name %q must match %s", name, roleNameRe.String())
	}
	return nil
}

func validateGrantName(name string) error {
	if !grantRe.MatchString(name) {
		return fmt.Errorf("agora roles: grant %q must match kind:verb:name (lowercase kind+verb, registry name, no wildcards)", name)
	}
	return nil
}
