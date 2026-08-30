@echo off
REM Windows convenience helper. The primary (Ubuntu) path is ./up.sh — see README.md.
set ROOT=C:\Users\game\Documents\ai\cold-memory-retrieval\chat-retrieval-system

echo ==== Starting model services (embedder + reranker) ====
REM up -d idempotent: healthy containers untouched. fetch-embedder/fetch-reranker
REM wget the GGUF weights into the chat_models volume on first run, then the two
REM llama-server instances start. Full reset = manual down.
cd /d "%ROOT%\gpu-server"
docker compose up -d

echo ==== Starting main stack (PostgreSQL + app) ====
cd /d "%ROOT%"
docker compose up -d --build

echo ==== Status ====
docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}"

echo.
echo Done. First run downloads the GGUF models (~2.3GB) into the chat_models
echo volume; after that llama.cpp starts fast.
pause