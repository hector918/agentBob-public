package agora

import "gopkg.in/yaml.v3"

// Default seed templates for a freshly-created Company. Ported from skeleton
// agora/30-config.go — but the disk-side $BOB_HOME/agora/permissions.yaml seed
// machinery (ReadSeedYAML / SeedYAMLIfMissing / LoadRoles) is DROPPED: trunk
// operations seed the per-Company permissions_yaml column directly from these
// in-process constants (the company column IS the source of truth, edited via
// the webui yaml editor — docs/agora-port.md §3). No file is read at create
// time.

// SeededPermissionsYAML is the starter per-role grants matrix copied into a new
// Company's permissions_yaml column at create time. Same shape as
// $BOB_HOME/permissions.yaml (the matrix the hub admin reads at a glance):
// `roles:` declares the role names, `grants:` is a capability → role → on|off
// map. Admin prunes per-role lines down to exactly what each role needs.
//
// The grant list mirrors the live trunk tool/skill registries (clarify / now /
// fs / terminal / skill_view / delegate / deliver_file / web_search_scrapling
// / vision / audio / send_message). Adding a tool/skill to the registry does NOT auto-grant
// it to existing Companies — their snapshot column is edited via the webui.
const SeededPermissionsYAML = `# agora 角色授权矩阵（每个新公司创建时复制一份独立快照）。
#
# 心智模型 —— 矩阵化授权:
#   - roles:  顶层列出本公司有哪些 role
#   - grants: 每个工具 / skill 一个 block，per role 标 on / off
#
# 不支持通配；编辑本 yaml 只影响本公司（每个公司各 carry 一份独立快照）。

roles:
  - boss
  - member
  - intern

grants:
  # Tools — alphabetical. 默认三个 role 一致 on，admin 之后按 role 收紧。
  tool:use:clarify:            { boss: on, member: on, intern: on }
  tool:use:delegate:           { boss: on, member: on, intern: on }
  tool:use:deliver_file:       { boss: on, member: on, intern: on }
  tool:use:fs:                 { boss: on, member: on, intern: on }
  tool:use:now:                { boss: on, member: on, intern: on }
  tool:use:audio:              { boss: on, member: on, intern: on }
  tool:use:vision:             { boss: on, member: on, intern: on }
  tool:use:send_message:       { boss: on, member: on, intern: on }
  tool:use:skill_view:         { boss: on, member: on, intern: on }
  tool:use:terminal:           { boss: on, member: on, intern: on }
  tool:use:web_search_scrapling: { boss: on, member: on, intern: on }
`

// SeededCompanyPlaybook is the default per-Company operating manual copied into
// bob_agora_companies.playbook at create time. PLAIN PROSE — a collaboration map
// read by EVERY agora role, injected into the identity layer next to the
// directory. Never parsed / validated; admin edits it freely via the webui.
const SeededCompanyPlaybook = `# 本公司运作说明

这是给本公司每一位成员看的协作地图:我们怎么分工、谁负责什么、
遇到事情该找谁。把它当成上岗第一天的入职手册——里面写的是「规范」,
不是某一个人的技能清单。

> 管理员请按本公司实际情况改写下面的内容(尤其是「谁负责什么」一节)。
> 这段文字可以随时在管理界面里直接编辑,改完对全公司所有角色即时生效。

## 谁负责什么

- (占位)写清楚本公司有哪些职责板块,各自由谁/哪个角色牵头。
- 不确定某件事归谁时:发给最相关的同事,并在消息里说清你的目标,让对方
  判断是自己接还是转交。

## 什么事找谁

- 需要别人配合时,用 send_message,to= 填对方的 inbox_scope
  (上方通讯录里列了你现在能联系到谁)。
- 对上汇报 / 把成果交回发起方:走 bridge inbox(上方身份说明里给了地址)。

## 协作的基本节奏

1. **看清需求** —— 弄明白到底要交付什么、需要哪些能力。
2. **拆解** —— 能独立交付的部分各自拆开。
3. **分工 / 派活** —— 自己能干的就干;需要别人的,用 send_message 把任务发过去。
4. **等待** —— 派出去之后本轮自然收尾;对方有结果会回到你的 inbox。
5. **收齐** —— 把各方回来的结果拼到一起,标明哪部分是谁做的。
6. **接力或回报** —— 还有后续就继续派;全部齐活就拼装好交回发起方。
`

// FirstRoleNameInYAML returns the first role name under the top-level `roles:`
// section of a permissions.yaml body, preserving YAML source order. Handles both
// the matrix format (`roles:` is a SEQUENCE) and the legacy mapping format.
// Empty body / missing-or-empty `roles:` / a scalar value all return "". Used by
// Bootstrap to pick the founder's role without hardcoding a name.
func FirstRoleNameInYAML(body []byte) (string, error) {
	if len(body) == 0 {
		return "", nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		return "", err
	}
	cur := &root
	if cur.Kind == yaml.DocumentNode {
		if len(cur.Content) == 0 {
			return "", nil
		}
		cur = cur.Content[0]
	}
	if cur == nil || cur.Kind != yaml.MappingNode {
		return "", nil
	}
	for i := 0; i+1 < len(cur.Content); i += 2 {
		k := cur.Content[i]
		v := cur.Content[i+1]
		if k.Value != "roles" {
			continue
		}
		if v == nil || (v.Kind != yaml.SequenceNode && v.Kind != yaml.MappingNode) || len(v.Content) == 0 {
			return "", nil
		}
		return v.Content[0].Value, nil
	}
	return "", nil
}
