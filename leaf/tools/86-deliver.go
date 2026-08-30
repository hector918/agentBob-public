package tools

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"agentbob/contract"
)

// deliverFile sends a file from the turn's work space to the user as a platform
// attachment. The bytes never enter the conversation — the file is resolved to a
// real local path inside the (authorized) space and handed to the sink's one-off
// file sender, which is bound to this turn's chat. Independent of the text reply.
type deliverFile struct{}

const (
	// maxDeliverBytes caps the file size we hand to a platform attachment send —
	// chat platforms reject oversized uploads anyway, so refuse early with a clear
	// message rather than a raw platform error.
	maxDeliverBytes = 10 << 20 // 10 MiB
	// maxCaptionRunes bounds the caption so a runaway model can't push a whole
	// document into the attachment caption.
	maxCaptionRunes = 800
)

func (deliverFile) Spec() contract.ToolSpec {
	return contract.ToolSpec{
		Name:        "deliver_file",
		Description: "把工作空间里的文件作为附件发给用户。path=空间内相对路径；space 留空=默认空间；caption=可选说明。",
		Delivers:    true, // a successful send to the user is production (§6.4)
		SideEffect:  true, // a failed delivery the model may still claim as done (§6.3.4)
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":    {"type": "string", "description": "要发送的文件，空间内的相对路径，例如 report.pdf 或 out/图表.png"},
    "space":   {"type": "string", "description": "可选：空间名；留空=默认空间"},
    "caption": {"type": "string", "description": "可选：随附件一起发的一行说明"}
  },
  "required": ["path"]
}`),
	}
}

func (deliverFile) Serialize() bool { return false }

func (deliverFile) Run(ctx context.Context, tc contract.ToolContext, args json.RawMessage) contract.ToolResult {
	var a struct {
		Path    string `json:"path"`
		Space   string `json:"space"`
		Caption string `json:"caption"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return contract.ErrResult("参数解析失败：" + err.Error())
	}
	if strings.TrimSpace(a.Path) == "" {
		return contract.ErrResult("缺少 path")
	}
	if tc.Channels == nil {
		return contract.ErrResult("当前没有可用的工作空间（无法取文件）")
	}
	if tc.Sink == nil {
		return contract.ErrResult("当前没有可用的发送通道（无法发文件给用户）")
	}

	ch, err := tc.Channels.OpenFile(ctx, a.Space)
	if err != nil {
		return contract.ErrResult("打开空间失败：" + err.Error())
	}
	defer ch.Close() // local file channel Close is a no-op, but the remote/sftp backend leaks a connection without it
	abs, err := ch.AbsPath(a.Path)
	if err != nil {
		return contract.ErrResult("找不到文件或路径越界：" + err.Error())
	}
	info, err := os.Stat(abs)
	if err != nil {
		return contract.ErrResult("读取文件失败：" + err.Error())
	}
	if info.IsDir() {
		return contract.ErrResult("不能发送目录，请指定一个文件")
	}
	if info.Size() > maxDeliverBytes {
		return contract.ErrResult("文件太大（>10 MiB）")
	}

	caption := truncateRunesEllipsis(a.Caption, maxCaptionRunes)
	if err := tc.Sink.SendFile(abs, caption); err != nil {
		return contract.ErrResult("发送失败：" + err.Error())
	}
	return contract.OKResult("已发送 " + a.Path)
}

// truncateRunesEllipsis clips s to at most max runes, appending "…" when it had
// to cut. max<=0 returns "".
func truncateRunesEllipsis(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
