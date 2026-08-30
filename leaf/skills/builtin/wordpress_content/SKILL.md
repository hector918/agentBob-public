---
name: wordpress_content
description: |
  Triggers: WordPress, WP, 博客, 文章, 发文章, 发博客, 草稿, 页面, 媒体, 上传图片, 分类, 标签, 评论, blog post, page, media, upload image, category, tag, comment
  WordPress 内容 REST 操作指引（文章 / 页面 / 媒体 / 分类 / 标签 / 评论的增删改查）。
  Use when: 用 wordpress_content 工具操作 WordPress 网站内容前先读它。
  Do NOT use for: WooCommerce 网店商品 / 订单（那是 woocommerce_operation）。
---

# WordPress 内容操作 skill

配套工具 **wordpress_content**（发 REST 请求）。**认证工具已自动处理**——你只管给 `method` + `path` + 可选 `query` / `body`。所有 path 都以 `/wp-json/wp/v2/` 开头。

## 通用约定
- 分页 / 控体积：`query` 用 `per_page`（默认 10，上限 100）、`page`、`_fields`、`search`、`status`、`orderby`。
- 状态码：2xx 成功；4xx 看返回 `{code, message}`。

## 文章 posts
| 操作 | method | path |
|---|---|---|
| 列表 | GET | `/wp-json/wp/v2/posts` |
| 单篇 | GET | `/wp-json/wp/v2/posts/<id>` |
| 新建 | POST | `/wp-json/wp/v2/posts` |
| 修改 | PUT | `/wp-json/wp/v2/posts/<id>` |
| 删除 | DELETE | `/wp-json/wp/v2/posts/<id>?force=true` |

常用字段：`title`、`content`（HTML / 块）、`excerpt`、`status`（`draft` / `publish` / `pending` / `private`）、`categories`（`[3,7]` 分类 id 数组）、`tags`（id 数组）、`featured_media`（特色图的媒体 id）、`slug`、`date`。

例 · 发草稿：POST `/wp-json/wp/v2/posts`，body `{"title":"夏季新品上市","content":"<p>正文…</p>","status":"draft","categories":[3]}`
例 · 发布已有草稿：PUT `/wp-json/wp/v2/posts/88`，body `{"status":"publish"}`

## 页面 pages / 分类 / 标签 / 评论
- 页面：`/wp-json/wp/v2/pages`（字段同 posts，多 `parent` / `menu_order`）。
- 分类：`/wp-json/wp/v2/categories`（`name` `slug` `parent`）；标签：`/wp-json/wp/v2/tags`。
- 评论：`/wp-json/wp/v2/comments`（`post` `content` `status`：approve / hold / spam / trash）。

### 标签 / 分类要 term id，不是名字
`tags` / `categories` 字段要的是 **term id（整数）数组**，不能直接塞名字字符串。取 id 的流程：
1. 查：GET `/wp-json/wp/v2/tags?search=风景` —— 命中就拿返回里的 `id`。
2. 查不到再建：POST `/wp-json/wp/v2/tags`，body `{"name":"风景"}` —— 拿返回的 `id`。
3. 把这些 id 收成数组，传给目标的 `tags`（如 `tags:[3,7]`）。分类同理走 `/wp-json/wp/v2/categories`。

## 媒体 media
- 列表 / 查：GET `/wp-json/wp/v2/media`；单个 GET `/wp-json/wp/v2/media/<id>`。
- **上传图片**：POST `/wp-json/wp/v2/media`，body 给 `{"file":"inbox/logo.png"}`（可选 `filename`，默认取文件名）。工具会读空间里的这个文件、以二进制上传，返回 JSON 里拿新媒体 `id`。
  - `file` 是**空间相对路径**：用户本轮发来的图在 `inbox/<文件名>`；也可以是你之前下载 / 生成、已存在空间里的文件。
  - title / alt_text / caption 这些元数据不在上传时设——先上传拿 `id`，再用下面的「改元数据」补。
  - 配图：把拿到的 `id` 填到文章的 `featured_media`。
- 改元数据：POST `/wp-json/wp/v2/media/<id>`，body 可含 `title`、`caption`（图片说明，**必须英文**）、`description`（用户给的描述**原样上传、一字不改**，除非用户特别要求）、`alt_text`、`post`（关联 / 父文章 id）、`tags`（term id 数组，取 id 见上）。
  - 例：POST `/wp-json/wp/v2/media/123`，body `{"caption":"Summer collection lookbook","description":"用户原文照抄","tags":[3,7],"post":456}`（caption 英文；description 原样）

## 注意
- 你不需要去看这个图片（不用调 `image`），因为后面有专门的流程去处理，你要做的是收集和提交用户给你的信息，
- 禁止主动去看图片（`image` 的任何 task），除非用户要求
- 如果你收到很多图，先记下图片文件名， 然后要把所有图一张张传上去，
- 发布/ 编辑/ 删除是对线上站点的真实写操作，先确认 id。
- **默认建草稿**：新建 post 一律 `status:"draft"`。只有用户**明确说了可以发布**才 `publish`；用户没说，就**主动问一句「要不要发布？」**，绝不擅自 `publish`。
- **description / prompt（描述）字段原样上传**：除非用户特别要求，用户给的描述文字**一字不改**照原样提交，不要润色、翻译、改写、补充。
- **caption（图片说明）和链接必须是英文**：不用中文或其它语言。
- 当用户传来图片或文字， 指定是要传到word press, 先确定用户的意图，确认拿到所有需要的文件， 文字， 和操作授权，材料，意图，ID，全部到位，后再由用户确定后进行操作。
- 所有编辑/ 修改/ 删除 都必定要确认过ID，才能操作


## 界面
- 在初始介绍时， 说明每次发布的组织形式， 是单一post 下面， 多个图片，
- 在介绍post时，需要说明那个是封面， 每个图片不能说名字，如果已经上传了的，必定把图片的链接带在对话中，用户用来确认


## 流程
- 正确流程 - 确保每次上传尊重这个流程，如果没有post_id，那么图片不会被处理，如果没有post_id, agent也不应该publish
- 如果你收到很多图，先记下图片文件名， 然后要把所有图一张张传上去，
1. 创建 post(图集):POST /wp/v2/posts { title, status:"draft" }   ← 默认 draft
   → 拿到 post_id
2. 例如上传三张图,每张:
   a. POST /wp/v2/media 上传 → 拿 image_id
   b. POST /wp/v2/media/{image_id} {
        "post": post_id,           ← 关键!建立 post_parent 关联
        "caption": "...",          ← 必须英文
        "description": "prompt 原样,不改;或留空",
        "tags": [term_ids]
      }
3. 设封面:POST /wp/v2/posts/{post_id} { "featured_media": 某image_id }
4. 发布:默认停在 draft。用户明确同意后才 PUT status=publish;用户没说就问「要不要发布?」——别擅自 publish