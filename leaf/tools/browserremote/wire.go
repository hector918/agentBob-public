package browserremote

import (
	"time"
)

// Wire request + result types, inlined from skeleton (browserd/wire.go +
// tools/browser action result structs). The shells decode browserd's response
// Data into these. Field tags are byte-identical to skeleton's so a browserd
// built from skeleton answers these requests verbatim — this port carries the
// MINIMAL set the 13 shells actually send/read, not the full browserd surface
// (no Ticket/Live/Profiles requests: those served the trunk-absent takeover +
// backup faces, dropped per the package doc).

// --- per-endpoint requests ---------------------------------------------------

type navigateReq struct {
	Common
	URL string `json:"url"`
}

type snapshotReq struct {
	Common
	Format   string `json:"format,omitempty"`
	Selector string `json:"selector,omitempty"`
}

type clickReq struct {
	Common
	Ref      string `json:"ref,omitempty"`
	Selector string `json:"selector,omitempty"`
}

type typeReq struct {
	Common
	Ref      string `json:"ref,omitempty"`
	Selector string `json:"selector,omitempty"`
	Text     string `json:"text"`
	Replace  bool   `json:"replace,omitempty"`
}

type pressReq struct {
	Common
	Key      string `json:"key"`
	Ref      string `json:"ref,omitempty"`
	Selector string `json:"selector,omitempty"`
}

type scrollReq struct {
	Common
	Direction string `json:"direction,omitempty"`
	Pixels    int    `json:"pixels,omitempty"`
	Selector  string `json:"selector,omitempty"`
}

type backReq struct {
	Common
}

type dialogReq struct {
	Common
	Accept     bool   `json:"accept"`
	PromptText string `json:"prompt_text,omitempty"`
}

type consoleReq struct {
	Common
	Level      string `json:"level,omitempty"`
	MaxEntries int    `json:"max_entries,omitempty"`
}

type getImagesReq struct {
	Common
	Selector  string `json:"selector,omitempty"`
	MaxImages int    `json:"max_images,omitempty"`
}

type visionReq struct {
	Common
}

type tabReq struct {
	Common
	Action string `json:"action,omitempty"`
	Index  int    `json:"index,omitempty"`
}

// purgeReq wipes the scope's browser state. Kept for PurgeSession (unwired in
// trunk — no /new purger chain yet).
type purgeReq struct {
	Scope string `json:"scope"`
}

// saveReq (保存登陆) writes the worker copy at scope back to its member master.
type saveReq struct {
	Scope string `json:"scope"`
}

// --- response payloads (browserd Response.Data) ------------------------------

// navigateResult — /navigate's Data (skeleton browser.NavigateResult).
type navigateResult struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Ready    bool   `json:"ready"`
	Snapshot string `json:"snapshot"`
}

// snapshotResult — /snapshot's Data (skeleton browser.SnapshotResult). Image
// carries the raw PNG (json []byte = base64 on the wire) when format=image.
type snapshotResult struct {
	Format         string `json:"format"`
	Selector       string `json:"selector,omitempty"`
	Content        string `json:"content,omitempty"`
	ScreenshotPath string `json:"path,omitempty"`
	Image          []byte `json:"image_b64,omitempty"`
	ByteCount      int    `json:"byte_count"`
	Truncated      bool   `json:"truncated"`
}

// backResult — /back's Data (skeleton browser.BackResult).
type backResult struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// consoleEntry — one /console entry (skeleton browser.ConsoleEntry).
type consoleEntry struct {
	When  time.Time `json:"when"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
	URL   string    `json:"url,omitempty"`
}

// consoleData — /console's Data (skeleton browserd.ConsoleData).
type consoleData struct {
	Entries []consoleEntry `json:"entries"`
}

// imageInfo — one /get-images entry (skeleton browser.ImageInfo).
type imageInfo struct {
	Src    string `json:"src"`
	Alt    string `json:"alt"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// imagesData — /get-images' Data (skeleton browserd.ImagesData).
type imagesData struct {
	Images []imageInfo `json:"images"`
}

// visionData — /vision's Data: the page as a base64 PNG (skeleton browserd.VisionData).
type visionData struct {
	ImageB64  string `json:"image_b64"`
	ByteCount int    `json:"byte_count"`
}

// --- takeover face wire (control-plane live / profiles reads) ------
// Byte-identical to browserd/wire.go so the deployed browserd answers verbatim.

type liveReq struct {
	Scope string `json:"scope"`
}
type liveData struct {
	Live bool `json:"live"`
}

type profilesReq struct{}
type profileInfo struct {
	Key    string `json:"key"`
	Owner  string `json:"owner"`
	Pinned bool   `json:"pinned"`
}
type profilesData struct {
	Profiles []profileInfo `json:"profiles"`
}
