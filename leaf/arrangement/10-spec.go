package arrangement

import (
	"encoding/json"
	"fmt"
	"strings"
)

// spec is the parsed arrangement orchestration: an ORDERED list of role buckets, each
// carrying free-form content (the instructions/要求 the member acts on). The engine
// reads only role + order; content is opaque (the member reads it). "完全弹性" =
// content can be anything; the only rule is one bucket per role, in order.
type spec struct {
	Buckets []bucket `json:"buckets"`
}

type bucket struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// parseSpec parses + validates a spec JSON (the Define gate). Validation is
// STRUCTURAL only: non-empty buckets, each with a role, roles distinct (one bucket per
// role). Whether the roles are staffed is NOT checked here — an unstaffed role's items
// become "unmet" at dispatch (docs/arrangement.md).
func parseSpec(raw string) (spec, error) {
	var s spec
	if strings.TrimSpace(raw) == "" {
		return spec{}, fmt.Errorf("编排 spec 为空")
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return spec{}, fmt.Errorf("编排 spec 不是合法 JSON：%w", err)
	}
	if len(s.Buckets) == 0 {
		return spec{}, fmt.Errorf("编排至少要有一个桶(buckets)")
	}
	seen := map[string]bool{}
	for i, b := range s.Buckets {
		if strings.TrimSpace(b.Role) == "" {
			return spec{}, fmt.Errorf("第 %d 个桶没有 role", i+1)
		}
		if seen[b.Role] {
			return spec{}, fmt.Errorf("role %q 出现了两次：每个 role 只能有一个桶", b.Role)
		}
		seen[b.Role] = true
	}
	return s, nil
}

// entryRole is the first bucket's role (where Inject drops new items).
func (s spec) entryRole() string {
	if len(s.Buckets) == 0 {
		return ""
	}
	return s.Buckets[0].Role
}

// indexOf returns the bucket index for role, or -1.
func (s spec) indexOf(role string) int {
	for i, b := range s.Buckets {
		if b.Role == role {
			return i
		}
	}
	return -1
}

// nextRole returns the role after `role` ("" → terminal / role unknown).
func (s spec) nextRole(role string) string {
	i := s.indexOf(role)
	if i < 0 || i+1 >= len(s.Buckets) {
		return ""
	}
	return s.Buckets[i+1].Role
}

// prevRole returns the role before `role` ("" → no upstream / role unknown).
func (s spec) prevRole(role string) string {
	i := s.indexOf(role)
	if i <= 0 {
		return ""
	}
	return s.Buckets[i-1].Role
}

// contentFor returns a bucket's authored content ("" when the role is unknown).
func (s spec) contentFor(role string) string {
	i := s.indexOf(role)
	if i < 0 {
		return ""
	}
	return s.Buckets[i].Content
}

// roles returns the bucket roles in order.
func (s spec) roles() []string {
	out := make([]string, 0, len(s.Buckets))
	for _, b := range s.Buckets {
		out = append(out, b.Role)
	}
	return out
}
