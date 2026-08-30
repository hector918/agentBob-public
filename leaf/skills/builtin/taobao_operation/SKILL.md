---
name: taobao_operation
description: |
  Triggers: 淘宝, 淘宝网, 淘宝店, 淘宝客服, 淘宝商品, 上淘宝, 逛淘宝, 浏览器(browser)打开淘宝网, Taobao, Taobao website, Taobao store, Taobao customer service, Taobao products, go to Taobao, browse Taobao, open Taobao website in a browser
  淘宝操作指引（商品搜索 / 产品资料收集 / 客服 / 数据查看等, 登陆）。
  Use when: 自主操作浏览器(browser)在淘宝 / 淘宝网上做相关操作时, 在动手操作淘宝前先读它, 。
  Do NOT use for: 其它电商平台（京东 / 拼多多 / 抖音小店等）；非店铺运营的普通淘宝购物搜索。
---

# 淘宝操作 skill
--工作流程--
> 产品资料收集： 产品资料收集任务，包括收集被选中的产品的名称，链接，价格，产品内页的图片，以及产品简介，产品详情，功能介绍，使用说明，都录到一个.md文档中，每个产品一个文档，图片也要下载下来，在fs工具中使用 agent的日期和产品名作为目录，

--问题解决--
> 遇到需要登录 / 验证码 / 二次验证、被登陆阻止时，调用 escalate_to_coo（kind=browser_takeover）把浏览器交给真人处理，本轮结束，等接管完成后再继续。If you encounter login, CAPTCHA, or two-factor authentication issues that you cannot bypass yourself, call escalate_to_coo (kind=browser_takeover) to hand over the browser to a human operator, end the current turn, and continue after the takeover is completed.

> 弹窗处理：操作过程中冒出的弹窗，**只有登录相关的**（要登录 / 验证码 / 二次验证）才按上面交给真人；**其余的弹窗你自己处理**——一般直接关掉就行（点关闭 / 「我知道了」/ 「稍后再说」/ 空白处），关掉后继续原来的任务，不用为这类弹窗打扰真人。For popups that appear during operation: only login-related ones (login / CAPTCHA / two-factor) get handed to a human as above; handle all others yourself — usually just close them (click close / "got it" / "later" / dismiss) and continue the task without bothering a human.


