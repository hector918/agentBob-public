package imagecreate

import "agentbob/contract"

// fakeCatalog stands in for the model side's shared style catalog. Two styles
// share one manual, mirroring how a model family's tiers really are documented.
type fakeCatalog struct{}

func (fakeCatalog) ImageStyles() []contract.ImageStyleInfo {
	return []contract.ImageStyleInfo{
		{Style: "comfyui-anima", Summary: "动漫 / 二次元。提示词用逗号分隔的标签。", ETA: "约 10 秒", Guide: "anima"},
		{Style: "comfyui-anima-hq", Summary: "动漫 / 二次元。提示词用逗号分隔的标签。", ETA: "约 5 分钟", Note: "慢档，质量更好", Guide: "anima"},
		{Style: "comfyui-klein", Summary: "写实 / 通用。用自然语言描述，越具体越好。", ETA: "约 30 秒", Guide: "flux2-klein"},
	}
}

func (fakeCatalog) ImageGuide(style string) (string, bool) {
	switch style {
	case "comfyui-anima", "comfyui-anima-hq":
		return "ANIMA MANUAL", true
	case "comfyui-klein":
		return "KLEIN MANUAL", true
	}
	return "", false
}

func testCatalog() contract.ImageCatalog { return fakeCatalog{} }

func fakeGuide(style string) string {
	body, _ := fakeCatalog{}.ImageGuide(style)
	return body
}
