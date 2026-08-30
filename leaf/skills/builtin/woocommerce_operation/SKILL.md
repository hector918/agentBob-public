---
name: woocommerce_operation
description: |
  Triggers: WooCommerce, woo, 网店, 网店运营, 商品, 上架, 改价, 库存, 订单, 发货, 订单状态, 客户, 会员, 优惠券, 退款, 销售报表, WordPress 商店, online store, products, orders, customers, coupons, refunds, sales report
  WooCommerce 网店 REST 操作指引（商品 / 订单 / 客户 / 优惠券 / 退款 / 报表的增删改查）。
  Use when: 用 woocommerce 工具操作网店前先读它，拿到端点路径和字段。
  Do NOT use for: WordPress 博客文章 / 页面 / 媒体（那是 wordpress_content）；其它电商平台（淘宝 / 京东等）。
---

# WooCommerce 操作 skill

配套工具 **woocommerce**（发 REST 请求）。**认证工具已自动处理**——你只管给 `method` + `path` + 可选 `query` / `body`，不用碰任何 key。所有 path 都以 `/wp-json/wc/v3/` 开头。

## 通用约定
- **分页 / 控体积**：`query` 里用 `per_page`（默认 10，上限 100）、`page`、`_fields`（只取要的字段，如 `_fields=id,name,price,stock_quantity`）。列表大就务必用 `_fields` + `per_page`，别一次拉全字段。
- **搜索 / 过滤**：`search`、`status`、`orderby`、`order`、`after` / `before`（ISO 时间）、`include=1,2,3`。
- **价格一律是字符串**：`"regular_price": "99.00"`，不是数字。
- **状态码**：2xx 成功；4xx 看返回的 `{code, message}` 自己改。

## 商品 products
| 操作 | method | path |
|---|---|---|
| 列表 | GET | `/wp-json/wc/v3/products` |
| 单个 | GET | `/wp-json/wc/v3/products/<id>` |
| 新建 | POST | `/wp-json/wc/v3/products` |
| 修改 | PUT | `/wp-json/wc/v3/products/<id>` |
| 删除 | DELETE | `/wp-json/wc/v3/products/<id>?force=true` |
| 变体 | GET/POST/PUT | `/wp-json/wc/v3/products/<id>/variations` |
| 分类 | GET/POST | `/wp-json/wc/v3/products/categories` |

常用字段：`name`、`type`（simple / variable）、`regular_price`、`sale_price`、`description`、`short_description`、`sku`、`status`（draft / publish）、`manage_stock`（true 才用库存数）、`stock_quantity`、`categories`（`[{"id": 15}]`）、`images`（`[{"src": "https://…"}]`，第一张为主图）。

例 · 上架一个商品：
- POST `/wp-json/wc/v3/products`，body `{"name":"纯棉T恤","type":"simple","regular_price":"99.00","sku":"TS-001","manage_stock":true,"stock_quantity":50,"status":"publish","categories":[{"id":15}]}`

例 · 改价：PUT `/wp-json/wc/v3/products/123`，body `{"regular_price":"79.00"}`

## 订单 orders
| 操作 | method | path |
|---|---|---|
| 列表 | GET | `/wp-json/wc/v3/orders` |
| 单个 | GET | `/wp-json/wc/v3/orders/<id>` |
| 改（含状态/发货） | PUT | `/wp-json/wc/v3/orders/<id>` |

状态 `status`：`pending`（待付款）、`processing`（处理中 / 已付款）、`on-hold`（保留）、`completed`（完成 / 已发货）、`cancelled`、`refunded`、`failed`。
- 查今天待处理：GET `/wp-json/wc/v3/orders?status=processing&per_page=50&_fields=id,number,total,billing,line_items`
- 标记发货完成：PUT `/wp-json/wc/v3/orders/456`，body `{"status":"completed"}`
- 加物流备注：PUT 加 `{"customer_note":"已发顺丰 SF123…"}`，或建订单备注 POST `/wp-json/wc/v3/orders/<id>/notes`，body `{"note":"…","customer_note":true}`

## 客户 customers / 优惠券 coupons / 退款 refunds
- 客户：GET/POST/PUT `/wp-json/wc/v3/customers`（字段 `email` `first_name` `last_name` `billing` `shipping`）。
- 优惠券：POST `/wp-json/wc/v3/coupons`，body `{"code":"VIP10","discount_type":"percent","amount":"10"}`。
- 退款：POST `/wp-json/wc/v3/orders/<id>/refunds`，body `{"amount":"50.00","reason":"客户取消"}`。

## 报表 reports
- 销售：GET `/wp-json/wc/v3/reports/sales?date_min=2026-06-01&date_max=2026-06-22`
- 各状态订单数 / 商品总数：GET `/wp-json/wc/v3/reports/orders/totals`、`/wp-json/wc/v3/reports/products/totals`

## 批量 batch（一次多条，省往返）
POST `/wp-json/wc/v3/products/batch`，body `{"create":[…], "update":[{"id":1,"regular_price":"…"}], "delete":[99]}`。orders / customers / coupons 同理换 path。

## 注意
- **删除 / 改价 / 改订单状态是对线上店的真实写操作**，动手前确认对象 id 正确；拿不准先 GET 核一下。
- 改了东西就用对应字段在结果里复核（如改价后看返回的 `regular_price`），别只凭 2xx 就声称完成。
