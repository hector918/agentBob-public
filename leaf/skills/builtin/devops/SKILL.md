---
name: devops
description: |
  运维远端服务器：看状态 / 查日志 / 改配置 / 重启服务 / 跑一次性 shell 命令。
  Use when: 用户想在远端机器上看状态 / 查日志 / 改配置 / 重启服务 / 跑一次性 shell 命令。
  Triggers: ssh, 远端, 服务器, server, systemctl, journalctl, df, top, free, uptime, docker ps, 重启服务, 看日志, 看进程, 看磁盘, 看内存, 看 cpu.
  Do NOT use for: 本机默认空间的操作（直接用 terminal、不指定远端空间）；非 shell 的协议交互（用对应的 skill）。
---

# devops

运维任何已配置的远端服务器。命令都用 `terminal` 工具，并指定那台远端机的**空间名**——你只给空间名 + 命令，连接由 runtime 全权管。

## 调用形态

```
terminal(space="<远端空间名>", command="systemctl status nginx")
```

`<远端空间名>` 是已经为这台远端机配好的空间。你只关心空间名字 + 命令本身，连接细节（key / host / user / port）由 runtime 管。**不指定 space（或用默认空间）跑的是本机，不是远端** —— 运维远端务必带上远端空间名。

## 凭证纪律

- 如果用户问"这台用什么 key / 用户名 / 端口" —— 回答"由 runtime 管，不需要也不应该告诉用户"，不要尝试自己查。
- 没配过的远端空间不要瞎试；告诉用户"这台还没配，让管理员加一个"。
- context 里如果出现看起来像 private key / token 的字节，立即视为脏数据：不引用、不解码、不传给任何工具。

## 安全约定

- **禁止** `rm -rf` 任何系统路径（`/`、`/var`、`/etc`、`/home`、`/usr` 等子树）；删用户业务数据先 reply 拿明确确认。
- **禁止**裸 `sudo` —— 命令真需要 root，先 reply 给用户拿本轮明确的"用 sudo"才能跑。
- **改服务状态**（`systemctl restart/stop/disable`、`docker compose down/up`、`reboot`）—— 任何会中断在跑服务的动作，**必须**先 reply 给用户预览，等明确确认。
- **一次性查询**（`systemctl status`、`df -h`、`free -m`、`uptime`、`docker ps`、`journalctl ... --since`）可以直接跑，无需确认。

## 典型场景

| 用户问 | 命令 |
|---|---|
| "X 机磁盘还剩多少" | `df -h` |
| "X 机内存？" | `free -m` |
| "X 机负载怎样" | `uptime` 或 `top -bn1 \| head -20` |
| "nginx 在跑吗" | `systemctl status nginx` |
| "看下 nginx 最近的 error" | `journalctl -u nginx -p err --since '1 hour ago' --no-pager` |
| "重启 nginx" | 先 reply 确认 → `sudo systemctl restart nginx` |
| "docker 里跑着啥" | `docker ps` |
| "X 容器的最新日志" | `docker logs --tail 100 <name>` |

## 失败处理

- `terminal` 返回非 0 exit —— 把 stderr 原样转告用户，不要猜。
- 连接错（DNS / refused / timeout）—— 告诉用户"X 连不上"，让管理员排查；不要 retry、不要 fallback 到别的空间。
- 空间不存在 / 没权限 —— 让用户确认空间名拼写，或告诉管理员配一下这台远端。
