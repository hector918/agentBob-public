@echo off
REM Windows convenience helper. The primary (Ubuntu) path is ./up.sh — see README.md.
cd /d %~dp0
git pull
docker compose up -d --build app