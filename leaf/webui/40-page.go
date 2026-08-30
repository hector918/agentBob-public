package webui

import _ "embed"

// appJS is the frontend, embedded at build time: the full canvas app (intro /
// dock / drill layers / takeover viewport) rendering the panels served by
// /api/state with the fixed field vocabulary. See docs/webui-trunk.md.
//
//go:embed app.js
var appJS []byte

// tosHTML / privacyHTML are the Discord application's legal pages, served
// UNAUTHENTICATED at /legal/* so Discord (and users) can fetch them. Source +
// placeholders to fill: leaf/webui/legal/README.md.
//
//go:embed legal/terms-of-service.html
var tosHTML []byte

//go:embed legal/privacy-policy.html
var privacyHTML []byte

// pageHTML is the static shell. On the home page there are EXACTLY two elements —
// a full-window canvas the frontend repaints every frame, and the single dock
// (the "全场唯一输入框"). No other DOM is permitted: the three canvas forms
// (layer / model / drill) render entirely on the canvas; model editing borrows
// the dock (a <textarea> that grows multi-line in model mode). See
// docs/webui-trunk.md §五.
const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>bob</title>
<style>
  html,body { margin:0; height:100%; background:#070605; overflow:hidden; }
  #graph { display:block; width:100vw; height:100vh; }
  /* The single dock: a <textarea> that is one line in command mode and grows in
     model mode (the canvas leaves room for it). It is the ONLY non-canvas DOM. */
  #dock {
    position:fixed; top:14px; left:50%; transform:translateX(-50%);
    width:min(560px,88vw); height:34px; padding:7px 12px; box-sizing:border-box;
    background:rgba(15,13,11,0.82); color:#e8e2d6;
    border:1px solid #3a342c; border-radius:6px;
    font:13px ui-monospace,SFMono-Regular,Menlo,monospace; line-height:18px;
    outline:none; resize:none; overflow:hidden; white-space:pre;
    backdrop-filter:blur(3px);
  }
  #dock.bottom { top:auto; bottom:14px; }
  #dock.model { width:min(820px,92vw); height:min(60vh,520px); overflow:auto; border-color:#4a7a5e; }
  #dock::placeholder { color:#6f7c93; }
</style>
</head>
<body>
<canvas id="graph"></canvas>
<textarea id="dock" rows="1" autocomplete="off" spellcheck="false" placeholder="paste a /webui token to unlock management"></textarea>
<script src="/app.js"></script>
</body>
</html>
`

// introMD is the editorial splash text served at /api/intro. Minimal default;
// a deploy may serve a richer file later.
const introMD = "# bob\n\n## a quiet engine\n\nthree bodies, one orbit.\n"
