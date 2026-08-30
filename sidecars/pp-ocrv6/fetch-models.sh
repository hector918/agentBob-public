#!/bin/sh
# Download PP-OCRv6 *medium* det+rec ONNX and extract the rec charset → keys.txt,
# into the models dir. The Dockerfile runs this at build, so a plain `docker build`
# produces a ready image — no manual model drop-in.
#
# Idempotent: a file already present is left alone, so you can pre-place your own
# (e.g. the tiny/small variant) to override, and re-runs are cheap.
#
# Needs: wget + python3 with PyYAML (both present in the build image after
# `pip install -r requirements.txt`). To use a different size, swap "medium" in
# the three URLs below for "small" or "tiny".
set -e

DIR="${1:-$(CDPATH= cd "$(dirname "$0")" && pwd)/models}"
mkdir -p "$DIR"

DET_URL=https://huggingface.co/PaddlePaddle/PP-OCRv6_medium_det_onnx/resolve/main/inference.onnx
REC_URL=https://huggingface.co/PaddlePaddle/PP-OCRv6_medium_rec_onnx/resolve/main/inference.onnx
RECYML_URL=https://huggingface.co/PaddlePaddle/PP-OCRv6_medium_rec_onnx/resolve/main/inference.yml

[ -f "$DIR/det.onnx" ] || { echo "↓ det.onnx"; wget -q -O "$DIR/det.onnx" "$DET_URL"; }
[ -f "$DIR/rec.onnx" ] || { echo "↓ rec.onnx"; wget -q -O "$DIR/rec.onnx" "$REC_URL"; }

# The rec charset isn't a standalone file — it's inline in inference.yml under
# PostProcess.character_dict. Extract it to one-char-per-line keys.txt (what
# RapidOCR's rec_keys_path wants). Search the key recursively in case the yml
# nesting differs across releases.
if [ ! -f "$DIR/keys.txt" ]; then
    echo "↓ rec inference.yml → keys.txt"
    wget -q -O "$DIR/rec.yml" "$RECYML_URL"
    python3 - "$DIR/rec.yml" "$DIR/keys.txt" <<'PY'
import sys, yaml
def find_dict(o):
    if isinstance(o, dict):
        for k, v in o.items():
            if k == "character_dict" and isinstance(v, list):
                return v
            r = find_dict(v)
            if r is not None:
                return r
    elif isinstance(o, list):
        for v in o:
            r = find_dict(v)
            if r is not None:
                return r
    return None
y = yaml.safe_load(open(sys.argv[1], encoding="utf-8"))
chars = find_dict(y)
if not chars:
    sys.exit("fetch-models: character_dict not found in rec inference.yml")
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.write("\n".join(str(c) for c in chars) + "\n")
print(f"keys.txt: {len(chars)} chars")
PY
    rm -f "$DIR/rec.yml"
fi

echo "PP-OCRv6 models ready in $DIR"
