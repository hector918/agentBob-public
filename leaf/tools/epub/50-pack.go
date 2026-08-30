package epub

// 50-pack.go — mode=pack: repackage the original epub with the translated (and
// possibly agent-edited) chapters from the work dir substituted in, write the new
// epub into the space, and tell the agent to deliver_file it. pack is pure file
// ops — no model call — so it works even when the translate backend is offline.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"

	"agentbob/contract"
)

func (t epubTool) runPack(ctx context.Context, tc contract.ToolContext, fileName, targetLang string) contract.ToolResult {
	if tc.Channels == nil {
		return contract.ErrResult("epub: 工作空间不可用")
	}
	ch, err := tc.Channels.OpenFile(ctx, "")
	if err != nil {
		return contract.ErrResult("epub: 打开工作空间失败：" + err.Error())
	}
	defer ch.Close()

	att, zr, errRes := t.openEpubZip(ctx, ch, tc.Attachments, fileName)
	if errRes != nil {
		return *errRes
	}
	defer zr.Close()

	book, err := parseBook(&zr.Reader)
	if err != nil {
		return contract.ErrResult("epub: " + err.Error())
	}

	// Only spine chapters are eligible replacements — a stray work-dir file with no
	// matching original entry is ignored, so it cannot corrupt the output.
	workDir := translateWorkDir(att, targetLang)
	replacements := map[string][]byte{}
	missing := 0
	for _, c := range book.Chapters {
		data, rerr := ch.Read(ctx, path.Join(workDir, c.File))
		if rerr != nil || len(data) == 0 {
			missing++ // never translated (skipped chapter) — keep the original verbatim
			continue
		}
		replacements[c.File] = data
	}
	// Chapter count is taken BEFORE the nav documents join the map: chapters and
	// kept_original are reported against the spine, and a nav doc is neither.
	chapters := len(replacements)

	// The navigation documents sit outside the spine, so they are not in the loop
	// above — pick up whatever translateTOC left in the work dir. Absent (older work
	// dir, or a book with no TOC) simply keeps the original.
	for _, nav := range []string{book.NavDoc, book.NCXDoc} {
		if nav == "" {
			continue
		}
		if _, done := replacements[nav]; done {
			continue
		}
		if data, rerr := ch.Read(ctx, path.Join(workDir, nav)); rerr == nil && len(data) > 0 {
			replacements[nav] = data
		}
	}
	if len(replacements) == 0 {
		return contract.ErrResult("epub: 工作空间里没有译好的章节——请先 mode=translate（file_name 与 target_lang 须与本次一致）")
	}

	packed, err := repackEpubBytes(&zr.Reader, replacements)
	if err != nil {
		return contract.ErrResult("epub: 打包失败：" + err.Error())
	}
	outPath := bookStem(att) + "." + sanitizeLangSegment(targetLang) + ".epub"
	if werr := ch.Write(ctx, outPath, packed); werr != nil {
		return contract.ErrResult("epub: 写入工作空间失败：" + werr.Error())
	}

	note := "打包完成，新 epub 已写入工作空间（见 output_path）。用 deliver_file 把它发给用户即可，然后简短告知已完成——不要再解压/打包。"
	if missing > 0 {
		note += fmt.Sprintf("注意：有 %d 章未翻译、保留了原文，打包前请告知用户。", missing)
	}
	res, _ := json.Marshal(map[string]any{
		"output_path": outPath, "chapters": chapters, "kept_original": missing, "note": note,
	})
	return contract.OKResult(string(res)).WithHint("用 deliver_file(path=output_path) 把成书发给用户。")
}

// repackEpubBytes writes a new epub into memory: entries in replacements
// (archive path → new bytes) come from the supplied bytes, every other entry is
// copied verbatim from the source zip. The "mimetype" entry, if present, is
// written first and STORED (uncompressed) per the OCF spec. Ported from
// skeleton's repackEpub, writing to a buffer instead of an os file.
func repackEpubBytes(zr *zip.Reader, replacements map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// Cumulative decompression budget, mirroring the read path's: the per-entry cap
	// alone bounds ONE entry, but repack holds the WHOLE book in memory at once, so a
	// book of many near-cap entries could still OOM the process. Counts translated
	// chapters too — they are equally in the buffer.
	budget := maxUnpackTotalBytes

	ordered := make([]*zip.File, 0, len(zr.File))
	var mimeEntry *zip.File
	for _, f := range zr.File {
		if f.Name == "mimetype" {
			mimeEntry = f
			continue
		}
		ordered = append(ordered, f)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	writeEntry := func(f *zip.File) error {
		var data []byte
		if repl, ok := replacements[f.Name]; ok {
			data = repl
		} else {
			// Verbatim copy of a source entry — bounded by the same per-entry cap as
			// zipBytes so a zip-bomb entry can't OOM the repack (header size is a hint;
			// the LimitReader is the real guard).
			if f.UncompressedSize64 > uint64(maxEntryUnpackBytes) {
				return fmt.Errorf("src entry %s too large (%d bytes, cap %d)", f.Name, f.UncompressedSize64, maxEntryUnpackBytes)
			}
			rc, oerr := f.Open()
			if oerr != nil {
				return fmt.Errorf("open src entry %s: %w", f.Name, oerr)
			}
			data, oerr = io.ReadAll(io.LimitReader(rc, maxEntryUnpackBytes+1))
			rc.Close()
			if oerr != nil {
				return fmt.Errorf("read src entry %s: %w", f.Name, oerr)
			}
			if int64(len(data)) > maxEntryUnpackBytes {
				return fmt.Errorf("src entry %s exceeds the per-entry size cap (%d bytes)", f.Name, maxEntryUnpackBytes)
			}
		}
		if int64(len(data)) > budget {
			return fmt.Errorf("repack exceeds the total size cap (%d bytes) at entry %s", maxUnpackTotalBytes, f.Name)
		}
		budget -= int64(len(data))
		method := zip.Deflate
		if f.Name == "mimetype" {
			method = zip.Store
		}
		hdr := &zip.FileHeader{Name: f.Name, Method: method}
		hdr.SetMode(0o644)
		w, werr := zw.CreateHeader(hdr)
		if werr != nil {
			return fmt.Errorf("create out entry %s: %w", f.Name, werr)
		}
		if _, werr := w.Write(data); werr != nil {
			return fmt.Errorf("write out entry %s: %w", f.Name, werr)
		}
		return nil
	}

	if mimeEntry != nil {
		if err := writeEntry(mimeEntry); err != nil {
			_ = zw.Close()
			return nil, err
		}
	}
	for _, f := range ordered {
		if err := writeEntry(f); err != nil {
			_ = zw.Close()
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("finalize output epub: %w", err)
	}
	return buf.Bytes(), nil
}
