package browser

import "agentbob/sidecars/browser/core"

// Exported ToolSpec accessors — the browserd thin client (tools/browserremote)
// reuses these verbatim so the model-facing surface (name / 中文 description /
// parameter schema / NoAutoCompress) is byte-identical between the in-process
// tools and the remote shells, with a single source of truth in tools.go.
// The Spec() methods never touch the pool, so the zero-value receivers are
// safe here.

func SpecNavigate() core.ToolSpec  { return browserNavigate{}.Spec() }
func SpecSnapshot() core.ToolSpec  { return browserSnapshot{}.Spec() }
func SpecClick() core.ToolSpec     { return browserClick{}.Spec() }
func SpecType() core.ToolSpec      { return browserType{}.Spec() }
func SpecPress() core.ToolSpec     { return browserPress{}.Spec() }
func SpecScroll() core.ToolSpec    { return browserScroll{}.Spec() }
func SpecBack() core.ToolSpec      { return browserBack{}.Spec() }
func SpecConsole() core.ToolSpec   { return browserConsole{}.Spec() }
func SpecGetImages() core.ToolSpec { return browserGetImages{}.Spec() }
func SpecDialog() core.ToolSpec    { return browserDialog{}.Spec() }
func SpecCDP() core.ToolSpec       { return browserCDP{}.Spec() }
func SpecVision() core.ToolSpec    { return browserVision{}.Spec() }
func SpecTab() core.ToolSpec       { return browserTab{}.Spec() }

// URLExcerpt exposes excerptFrom for the remote client, so both modes feed
// the URL library the same byte-capped excerpt.
func URLExcerpt(s string) string { return excerptFrom(s) }
