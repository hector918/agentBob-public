package warrant

import (
	"context"
	"testing"

	"agentbob/contract"
)

// stubSkillCat is a minimal contract.SkillCatalog for projection tests.
type stubSkillCat struct {
	skills map[string]contract.Skill
	bodies map[string]string
}

func (s stubSkillCat) List() []contract.SkillInfo {
	out := make([]contract.SkillInfo, 0, len(s.skills))
	for _, sk := range s.skills {
		out = append(out, sk.SkillInfo)
	}
	return out
}
func (s stubSkillCat) Get(name string) (contract.Skill, bool) {
	sk, ok := s.skills[name]
	return sk, ok
}
func (s stubSkillCat) Read(name string) (string, bool) { b, ok := s.bodies[name]; return b, ok }
func (s stubSkillCat) Files(name string) ([]contract.SkillFile, error) {
	if _, ok := s.skills[name]; !ok {
		return nil, nil
	}
	return []contract.SkillFile{{Rel: "scripts/x.py", Data: []byte("x")}}, nil
}

// TestAuthorizeSkills: the projection drops skills the principal can't use, and the
// narrowed set's Get/Read pass through only for authorized names.
func TestAuthorizeSkills(t *testing.T) {
	cat := stubSkillCat{
		skills: map[string]contract.Skill{
			"a": {SkillInfo: contract.SkillInfo{Name: "a"}},
			"b": {SkillInfo: contract.SkillInfo{Name: "b"}},
		},
		bodies: map[string]string{"a": "body-a", "b": "body-b"},
	}
	pol := fromFile(policyFile{
		Principals: []string{"admin", "user"},
		Grants: map[string]map[string]string{
			"skill:use:a": {"admin": "on", "user": "on"},
			"skill:use:b": {"admin": "on", "user": "off"},
		},
	})
	m := &Manager{skillCat: cat}
	m.policy.Store(pol)
	ctx := context.Background()

	// admin: both a and b.
	admin := m.AuthorizeSkills(ctx, m.Grants(ctx, "admin"), cat.List())
	if len(admin.List()) != 2 {
		t.Fatalf("admin should see 2 skills, got %d", len(admin.List()))
	}

	// user: only a. b is dropped from the list AND blocked at Get/Read even by name.
	user := m.AuthorizeSkills(ctx, m.Grants(ctx, "user"), cat.List())
	if len(user.List()) != 1 || user.List()[0].Name != "a" {
		t.Fatalf("user should see only [a], got %+v", user.List())
	}
	if body, ok := user.Read("a"); !ok || body != "body-a" {
		t.Errorf("user.Read(a) = %q,%v; want body-a", body, ok)
	}
	if _, ok := user.Read("b"); ok {
		t.Error("user.Read(b) must be blocked — visibility == usability")
	}
	if _, ok := user.Get("b"); ok {
		t.Error("user.Get(b) must be blocked")
	}
}
