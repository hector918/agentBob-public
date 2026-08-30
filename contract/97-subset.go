package contract

import "fmt"

// ToolSubset and SkillSubset are the shared projection wrappers that an authorizer
// builds AFTER applying its own policy filter: warrant (permissions matrix) and
// agora (company grants ∩ inbox visibility) each decide WHICH tools/skills an
// identity may use, then hand the survivors to one of these to expose as a
// ToolSet / SkillSet. The projection logic (membership + catalog passthrough) is
// identity-agnostic, so it lives here once instead of byte-for-byte in each leaf
// (leaf↔leaf imports are forbidden, so a shared leaf util isn't an option; these
// are pure constructors, same posture as OKResult/ErrResult).

// ToolSubset is a ToolSet over an explicitly-added subset of tools. Build with
// NewToolSubset, Add each authorized (spec, tool), then use it as a ToolSet.
type ToolSubset struct {
	byName map[string]Tool
	specs  []ToolSpec
}

// NewToolSubset returns an empty tool subset ready for Add.
func NewToolSubset() *ToolSubset { return &ToolSubset{byName: map[string]Tool{}} }

// Add includes one authorized tool (its advertised spec + the tool itself).
func (s *ToolSubset) Add(spec ToolSpec, tool Tool) {
	s.byName[spec.Name] = tool
	s.specs = append(s.specs, spec)
}

func (s *ToolSubset) Specs() []ToolSpec { return s.specs }
func (s *ToolSubset) Lookup(name string) (Tool, bool) {
	t, ok := s.byName[name]
	return t, ok
}

// SkillSubset is a SkillSet over an authorized subset: List returns the added
// infos; Get/Read/Files pass through to the catalog but ONLY for added names
// (visibility == usability). Build with NewSkillSubset(cat) + Add.
type SkillSubset struct {
	cat     SkillCatalog
	infos   []SkillInfo
	allowed map[string]struct{}
}

// NewSkillSubset returns an empty skill subset backed by cat (may be nil — then
// Get/Read/Files report no catalog). Add each authorized SkillInfo.
func NewSkillSubset(cat SkillCatalog) *SkillSubset {
	return &SkillSubset{cat: cat, allowed: map[string]struct{}{}}
}

// Add includes one authorized skill.
func (s *SkillSubset) Add(info SkillInfo) {
	s.infos = append(s.infos, info)
	s.allowed[info.Name] = struct{}{}
}

func (s *SkillSubset) List() []SkillInfo { return s.infos }

func (s *SkillSubset) Get(name string) (Skill, bool) {
	if s.cat == nil {
		return Skill{}, false
	}
	if _, ok := s.allowed[name]; !ok {
		return Skill{}, false
	}
	return s.cat.Get(name)
}

func (s *SkillSubset) Read(name string) (string, bool) {
	if s.cat == nil {
		return "", false
	}
	if _, ok := s.allowed[name]; !ok {
		return "", false
	}
	return s.cat.Read(name)
}

func (s *SkillSubset) Files(name string) ([]SkillFile, error) {
	if s.cat == nil {
		return nil, fmt.Errorf("no skill catalog")
	}
	if _, ok := s.allowed[name]; !ok {
		return nil, fmt.Errorf("skill %q not authorized", name)
	}
	return s.cat.Files(name)
}
