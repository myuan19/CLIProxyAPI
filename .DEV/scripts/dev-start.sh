#!/bin/bash
# dev-start.sh - 开发环境一键启动脚本 (Linux/Mac)
# 功能：构建前端 -> 复制到后端 -> 启动后端服务

set -e  # 遇到错误立即退出

# 获取脚本所在目录，从 .dev/scripts/ 往上两层到项目根目录，再往上一层到工作区根目录
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKEND_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
WORKSPACE_ROOT="$(cd "$BACKEND_DIR/.." && pwd)"
FRONTEND_DIR="$WORKSPACE_ROOT/Cli-Proxy-API-Management-Center"
BACKEND_STATIC_DIR="$BACKEND_DIR/static"
MANAGEMENT_HTML_PATH="$BACKEND_STATIC_DIR/management.html"

echo "========================================"
echo "  开发环境一键启动脚本"
echo "========================================"
echo ""

# 步骤 1: 检查前端目录
echo "[1/5] 检查前端项目..."
if [ ! -d "$FRONTEND_DIR" ]; then
    echo "错误: 找不到前端项目目录: $FRONTEND_DIR"
    exit 1
fi
echo "✓ 前端项目目录存在"

# 步骤 2: 检查并安装前端依赖
echo "[2/5] 检查前端依赖..."
cd "$FRONTEND_DIR"
if [ ! -d "node_modules" ]; then
    echo "  安装前端依赖..."
    npm install
    if [ $? -ne 0 ]; then
        echo "错误: 前端依赖安装失败"
        exit 1
    fi
else
    echo "✓ 前端依赖已存在"
fi

# 步骤 3: 构建前端
echo "[3/5] 构建前端项目..."
echo "  执行 npm run build..."
npm run build
if [ $? -ne 0 ]; then
    echo "错误: 前端构建失败"
    exit 1
fi

# 检查构建输出
DIST_INDEX_HTML="$FRONTEND_DIR/dist/index.html"
if [ ! -f "$DIST_INDEX_HTML" ]; then
    echo "错误: 构建输出文件不存在: $DIST_INDEX_HTML"
    exit 1
fi
echo "✓ 前端构建成功"

# 步骤 4: 复制文件到后端目录
echo "[4/5] 复制前端文件到后端..."
# 创建 static 目录（如果不存在）
mkdir -p "$BACKEND_STATIC_DIR"

# 复制文件
cp -f "$DIST_INDEX_HTML" "$MANAGEMENT_HTML_PATH"
echo "✓ 文件已复制到: $MANAGEMENT_HTML_PATH"

# 步骤 5: 设置环境变量并启动后端
echo "[5/5] 启动后端服务..."
echo ""
echo "设置环境变量: MANAGEMENT_STATIC_PATH=$MANAGEMENT_HTML_PATH"
export MANAGEMENT_STATIC_PATH="$MANAGEMENT_HTML_PATH"

echo ""
echo "========================================"
echo "  启动后端服务..."
echo "========================================"
echo ""
echo "前端管理界面: http://localhost:8317/management.html"
echo "  请求详情: http://localhost:8317/management.html#/detailed-requests"
echo ""
echo "按 Ctrl+C 停止服务"
echo ""

# 切换到后端目录并启动
cd "$BACKEND_DIR"

# 检查是否有配置文件，如果没有则从示例文件复制
CONFIG_FILE="$BACKEND_DIR/config.yaml"
CONFIG_EXAMPLE_FILE="$BACKEND_DIR/config.example.yaml"

if [ ! -f "$CONFIG_FILE" ]; then
    if [ -f "$CONFIG_EXAMPLE_FILE" ]; then
        echo "未找到 config.yaml，从 config.example.yaml 创建..."
        cp -f "$CONFIG_EXAMPLE_FILE" "$CONFIG_FILE"
        echo "✓ 已创建 config.yaml（从 config.example.yaml 复制）"
        echo "  提示: 请根据需要修改 config.yaml 中的配置"
    else
        echo "警告: 未找到 config.yaml 和 config.example.yaml，将使用默认配置"
    fi
fi

# 启动 Go 服务
go run cmd/server/main.go
