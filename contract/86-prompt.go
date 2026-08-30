package contract

// Prompt is the dumb, layered system-prompt builder: a set of named text layers
// held in insertion order, assembled into one system prompt. It knows NOTHING
// about what any layer means — the flow decides which layers exist, what fills
// them, and in what order (by call order). One builder per turn; it carries no
// conversation state. A flow fills the layers from context; a /prompt dump
// renders the assembled Build() output (Flow.DumpPrompt → compose.RenderDump).
type Prompt interface {
	// SetLayer sets (or replaces) the named layer's content. The first SetLayer
	// for a name fixes its position; a later SetLayer for the same name updates
	// the content in place. Empty content keeps the slot but drops it from Build.
	SetLayer(name, content string)
	// Build assembles the non-empty layers, in insertion order, into the system
	// prompt.
	Build() string
}

// PromptFactory hands out fresh builders. Registered on the trunk by the prompt
// module; a flow Requires it and calls New() once per turn. The factory holds no
// per-turn state — it is the "make a prompt" function the flow holds.
type PromptFactory interface {
	New() Prompt
}
