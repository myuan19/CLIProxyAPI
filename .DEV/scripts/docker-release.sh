#!/usr/bin/env bash
# docker-release.sh - Docker 构建、测试、发布一体化脚本
#
# 功能：构建 Docker 镜像 -> 运行容器检测 -> 访问网页确认 -> 清理容器 -> 上传到公开镜像仓库
# 支持在任意位置执行
#
# ========================================
# 脚本影响说明
# ========================================
# 本脚本会执行以下操作，可能影响你的系统：
#
# 1. 创建临时配置文件
#    - 影响: 在项目目录创建 config.test.yaml（如已存在会被覆盖）
#    - 位置: CLIProxyAPI/config.test.yaml
#    - 清理: 脚本结束时自动删除
#
# 2. 创建临时目录
#    - 影响: 创建 auths-test 和 logs-test 目录
#    - 位置: CLIProxyAPI/auths-test, CLIProxyAPI/logs-test
#    - 清理: 脚本结束时自动删除
#
# 3. 构建 Docker 镜像
#    - 影响: 在本地 Docker 创建镜像（占用磁盘空间，约 50-100 MB）
#    - 镜像名: 由 IMAGE_NAME 和 IMAGE_TAG 配置决定
#    - 清理: 可选（由 CLEANUP_LOCAL_IMAGE 配置决定）
#
# 4. 运行 Docker 容器
#    - 影响: 启动临时容器，占用端口 TEST_PORT（默认 18317）
#    - 容器名: cli-proxy-api-test
#    - 清理: 测试后自动停止并删除
#
# 5. 构建前端项目（可选，当 USE_LOCAL_FRONTEND=true 时）
#    - 影响: 在前端目录执行 npm install 和 npm run build
#    - 位置: Cli-Proxy-API-Management-Center/dist/index.html
#    - 说明: 构建的 HTML 会挂载到容器中，并设置 MANAGEMENT_STATIC_PATH 环境变量禁止自动更新
#
# 6. 推送到远程仓库（可选）
#    - 影响: 上传镜像到 Docker Hub 或其他仓库（需要提前 docker login）
#    - 需要: 网络连接和仓库写入权限

# 使用说明
# 保存脚本：将上面内容保存到 CLIProxyAPI/.dev/scripts/docker-release.sh
# 赋予执行权限：chmod +x docker-release.sh
# 任意位置运行脚本即可
# 配置变量（脚本开头可修改）：
# IMAGE_NAME - 镜像名称（默认 yuan019/cli-proxy-api）
# IMAGE_TAG - 镜像标签（默认 latest）
# PUSH_TO_REGISTRY - 是否推送（默认 true）
# TEST_PORT - 测试端口（默认 18317）
# USE_LOCAL_FRONTEND - 是否使用本地前端（默认 true）
#   - true: 构建本地前端项目，挂载到容器中，禁止从 GitHub 下载
#   - false: 使用 GitHub 自动下载的前端
# 推送前准备：
#    docker login  # 登录 Docker Hub

# ========================================

set -e

# ========================================
# 可配置变量（根据需要修改）
# ========================================

# Docker 镜像配置
IMAGE_NAME="yuan019/cli-proxy-api"           # 镜像名称（包含仓库前缀）
IMAGE_TAG="latest"                           # 镜像标签
PUSH_TO_REGISTRY=true                        # 是否推送到远程仓库
CLEANUP_LOCAL_IMAGE=false                    # 测试完成后是否删除本地镜像

# 前端配置
USE_LOCAL_FRONTEND=true                      # true: 使用本地构建的前端（默认）; false: 使用 GitHub 下载
FRONTEND_PROJECT_NAME="Cli-Proxy-API-Management-Center"  # 前端项目目录名

# 测试配置
TEST_PORT=18317                              # 测试用端口（避免与正在运行的服务冲突）
TEST_CONTAINER_NAME="cli-proxy-api-test"     # 测试容器名称
HEALTH_CHECK_TIMEOUT=60                      # 健康检查超时时间（秒）
HEALTH_CHECK_INTERVAL=2                      # 健康检查间隔（秒）
# 配置来源选项:
#   true:   使用系统配置 - 挂载真实目录（可以看到真实的认证文件）
#           配置基于 config.yaml，继承 providers、api-keys 等设置
#           挂载: 真实认证目录 + 真实日志目录
#   false:  使用测试配置 - 挂载临时目录（测试后自动清理）
#           配置为最小化设置，仅包含基本选项
#           挂载: 临时认证目录 + 临时日志目录
# 注意: 两种模式都会修改 auth-dir/secret-key 为测试值，方便容器内访问和登录
USE_SYSTEM_CONFIG=true

# 测试配置参数
TEST_API_KEY="test-api-key-for-docker-build"   # 仅 USE_SYSTEM_CONFIG=false 时使用
TEST_MANAGEMENT_KEY="test-management-key"      # 两种模式都使用（替换原密钥方便登录）

# 系统目录路径（当 USE_SYSTEM_CONFIG=true 时挂载真实目录）
# 修改这里以匹配你的系统配置
SYSTEM_AUTH_DIR_PATH="/root/.cli-proxy-api"  # 与 config.yaml 中的 auth-dir 对应

# 项目目录名（用于定位项目根目录）
PROJECT_DIR_NAME="CLIProxyAPI"

# ========================================
# 颜色定义
# ========================================
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
MAGENTA='\033[0;35m'
CYAN='\033[0;36m'
GRAY='\033[0;90m'
WHITE='\033[0;37m'
NC='\033[0m' # No Color

# ========================================
# 自动计算路径（不需要修改）
# ========================================
SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_PATH/../.." && pwd)"  # 从 .dev/scripts/ 往上两层到项目根目录

# 如果脚本路径计算出的不是项目目录，尝试查找
if [ "$(basename "$PROJECT_ROOT")" != "$PROJECT_DIR_NAME" ]; then
    # 尝试从当前目录往上查找
    search_dir="$(pwd)"
    while [ "$search_dir" != "/" ]; do
        if [ "$(basename "$search_dir")" = "$PROJECT_DIR_NAME" ]; then
            PROJECT_ROOT="$search_dir"
            break
        fi
        search_dir="$(dirname "$search_dir")"
    done

    # 仍然没找到，尝试当前目录下查找
    if [ "$(basename "$PROJECT_ROOT")" != "$PROJECT_DIR_NAME" ]; then
        if [ -d "$(pwd)/$PROJECT_DIR_NAME" ]; then
            PROJECT_ROOT="$(pwd)/$PROJECT_DIR_NAME"
        elif [ -d "$(dirname "$(pwd)")/$PROJECT_DIR_NAME" ]; then
            PROJECT_ROOT="$(dirname "$(pwd)")/$PROJECT_DIR_NAME"
        fi
    fi
fi

# 验证项目根目录
DOCKERFILE="$PROJECT_ROOT/Dockerfile"
if [ ! -f "$DOCKERFILE" ]; then
    echo -e "${RED}错误: 无法找到项目根目录。请在 CLIProxyAPI 目录或其子目录中运行此脚本。${NC}"
    echo -e "${YELLOW}当前检测路径: $PROJECT_ROOT${NC}"
    exit 1
fi

# 前端项目路径（用于本地前端构建）
WORKSPACE_ROOT="$(dirname "$PROJECT_ROOT")"
FRONTEND_DIR="$WORKSPACE_ROOT/$FRONTEND_PROJECT_NAME"
FRONTEND_DIST_HTML="$FRONTEND_DIR/dist/index.html"
CONTAINER_HTML_PATH="/CLIProxyAPI/static/management.html"

# 临时文件路径（仅当 USE_SYSTEM_CONFIG=false 时使用）
TEST_CONFIG_FILE="$PROJECT_ROOT/config.test.yaml"
TEST_AUTH_DIR="$PROJECT_ROOT/auths-test"
TEST_LOG_DIR="$PROJECT_ROOT/logs-test"
LOCAL_DOCKERFILE="$PROJECT_ROOT/Dockerfile.local"
BACKEND_STATIC_DIR="$PROJECT_ROOT/static"
BACKEND_MANAGEMENT_HTML="$BACKEND_STATIC_DIR/management.html"

# 系统目录路径（使用顶部配置的变量）
SYSTEM_AUTH_DIR="$SYSTEM_AUTH_DIR_PATH"
SYSTEM_LOG_DIR="$PROJECT_ROOT/logs"

# 完整镜像名称
FULL_IMAGE_NAME="${IMAGE_NAME}:${IMAGE_TAG}"

# ========================================
# 辅助函数 - 日志输出（使用彩色区分不同对象）
# ========================================
# 颜色规范:
#   Cyan    - 命令/步骤标题
#   Yellow  - 文件路径/目录
#   Green   - 成功消息
#   Red     - 错误消息
#   Magenta - 警告/重要提示
#   Gray    - 影响描述/次要信息
#   White   - 普通文本

write_command() {
    local command="$1"
    local description="${2:-}"
    echo ""
    echo -e "${BLUE}> ${CYAN}${command}${NC}"
    if [ -n "$description" ]; then
        echo -e "  ${GRAY}影响: ${description}${NC}"
    fi
}

write_step() {
    local step_number="$1"
    local step_name="$2"
    echo ""
    echo -e "${CYAN}========================================${NC}"
    echo -e "${CYAN}  [${step_number}] ${step_name}${NC}"
    echo -e "${CYAN}========================================${NC}"
}

write_filepath() {
    local label="$1"
    local filepath="$2"
    local extra="${3:-}"
    if [ -n "$extra" ]; then
        echo -e "  ${GRAY}${label} ${YELLOW}${filepath} ${GRAY}${extra}${NC}"
    else
        echo -e "  ${GRAY}${label} ${YELLOW}${filepath}${NC}"
    fi
}

write_info() {
    local label="$1"
    local value="$2"
    echo -e "  ${GRAY}${label} ${WHITE}${value}${NC}"
}

write_success() {
    echo -e "${GREEN}✓ $1${NC}"
}

write_warning() {
    echo -e "${MAGENTA}⚠ $1${NC}"
}

write_error() {
    echo -e "${RED}✗ $1${NC}"
}

# ========================================
# 清理函数
# ========================================
cleanup_test_resources() {
    echo ""
    echo -e "${YELLOW}清理测试资源...${NC}"

    # 停止并删除测试容器
    local container
    container=$(docker ps -aq -f "name=$TEST_CONTAINER_NAME" 2>/dev/null || true)
    if [ -n "$container" ]; then
        write_command "docker stop $TEST_CONTAINER_NAME" "停止测试容器"
        docker stop "$TEST_CONTAINER_NAME" 2>/dev/null || true
        write_command "docker rm $TEST_CONTAINER_NAME" "删除测试容器"
        docker rm "$TEST_CONTAINER_NAME" 2>/dev/null || true
    fi

    # 删除临时配置文件（两种模式都会创建）
    if [ -f "$TEST_CONFIG_FILE" ]; then
        write_command "rm \"$TEST_CONFIG_FILE\"" "删除临时配置文件"
        rm -f "$TEST_CONFIG_FILE"
    fi

    # 删除临时目录（仅当不使用系统配置时才创建，不删除系统真实目录）
    if [ "$USE_SYSTEM_CONFIG" != "true" ]; then
        if [ -d "$TEST_AUTH_DIR" ]; then
            write_command "rm -rf \"$TEST_AUTH_DIR\"" "删除临时认证目录"
            rm -rf "$TEST_AUTH_DIR"
        fi
        if [ -d "$TEST_LOG_DIR" ]; then
            write_command "rm -rf \"$TEST_LOG_DIR\"" "删除临时日志目录"
            rm -rf "$TEST_LOG_DIR"
        fi
    fi

    # 删除临时 Dockerfile
    if [ -f "$LOCAL_DOCKERFILE" ]; then
        write_command "rm \"$LOCAL_DOCKERFILE\"" "删除临时 Dockerfile"
        rm -f "$LOCAL_DOCKERFILE"
    fi

    # 删除临时 static 目录（仅在使用本地前端时创建）
    if [ -d "$BACKEND_STATIC_DIR" ]; then
        write_command "rm -rf \"$BACKEND_STATIC_DIR\"" "删除临时 static 目录"
        rm -rf "$BACKEND_STATIC_DIR"
    fi

    write_success "测试资源已清理"
}

# ========================================
# Ctrl+C 中断处理
# ========================================
CONTAINER_STARTED=false

trap_handler() {
    echo ""
    echo -e "${YELLOW}检测到中断信号 (Ctrl+C)...${NC}"
    if [ "$CONTAINER_STARTED" = "true" ]; then
        cleanup_test_resources
    fi
    exit 1
}
trap trap_handler INT TERM

# 同步配置文件（排除服务器配置字段）
sync_config_excluding_server_config() {
    local source_file="$1"    # config.test.yaml
    local target_file="$2"    # config.yaml

    if [ ! -f "$source_file" ]; then
        echo -e "  ${RED}✗ 源文件不存在: $source_file${NC}"
        return
    fi
    if [ ! -f "$target_file" ]; then
        echo -e "  ${RED}✗ 目标文件不存在: $target_file${NC}"
        return
    fi

    local source_content
    source_content=$(<"$source_file")
    local target_content
    target_content=$(<"$target_file")

    # 使用 sed 从源文件内容中保留目标文件的 host、port、tls、remote-management、auth-dir 字段
    # 由于 bash 正则处理复杂 YAML 比较困难，这里使用简化策略：
    # 直接复制源文件，然后用目标文件中的特定字段值替换回去

    local new_content="$source_content"
    local preserved_fields=""

    # 提取并保留 host 字段
    local target_host
    target_host=$(grep -E '^host:' "$target_file" 2>/dev/null || true)
    local source_host
    source_host=$(grep -E '^host:' "$source_file" 2>/dev/null || true)
    if [ -n "$target_host" ] && [ -n "$source_host" ] && [ "$target_host" != "$source_host" ]; then
        new_content=$(echo "$new_content" | sed "s|^host:.*|${target_host}|")
        preserved_fields="${preserved_fields}host, "
    fi

    # 提取并保留 port 字段
    local target_port
    target_port=$(grep -E '^port:' "$target_file" 2>/dev/null || true)
    local source_port
    source_port=$(grep -E '^port:' "$source_file" 2>/dev/null || true)
    if [ -n "$target_port" ] && [ -n "$source_port" ] && [ "$target_port" != "$source_port" ]; then
        new_content=$(echo "$new_content" | sed "s|^port:.*|${target_port}|")
        preserved_fields="${preserved_fields}port, "
    fi

    # 提取并保留 auth-dir 字段
    local target_authdir
    target_authdir=$(grep -E '^auth-dir:' "$target_file" 2>/dev/null || true)
    local source_authdir
    source_authdir=$(grep -E '^auth-dir:' "$source_file" 2>/dev/null || true)
    if [ -n "$target_authdir" ] && [ -n "$source_authdir" ] && [ "$target_authdir" != "$source_authdir" ]; then
        new_content=$(echo "$new_content" | sed "s|^auth-dir:.*|${target_authdir}|")
        preserved_fields="${preserved_fields}auth-dir, "
    fi

    # 检查是否有实际变化
    if [ "$new_content" = "$target_content" ]; then
        echo -e "  ${GRAY}✓ 无需同步（内容相同）${NC}"
        return
    fi

    # 写入目标文件
    echo "$new_content" > "$target_file"

    # 打印同步结果
    echo -e "  ${GREEN}✓ 配置已同步${NC}"
    echo -e "    ${GRAY}$source_file -> $target_file${NC}"

    # 显示保留的字段
    if [ -n "$preserved_fields" ]; then
        # 去掉末尾的 ", "
        preserved_fields="${preserved_fields%, }"
        echo -e "    ${MAGENTA}已保留原值: ${preserved_fields}${NC}"
    fi
}

# ========================================
# 主流程
# ========================================

echo ""
echo -e "${MAGENTA}╔════════════════════════════════════════════════════════════╗${NC}"
echo -e "${MAGENTA}║     Docker 构建、测试、发布一体化脚本                      ║${NC}"
echo -e "${MAGENTA}╚════════════════════════════════════════════════════════════╝${NC}"
echo ""
echo -e "${GRAY}配置信息:${NC}"
echo -e "${GRAY}  项目根目录: $PROJECT_ROOT${NC}"
echo -e "${GRAY}  镜像名称: $FULL_IMAGE_NAME${NC}"
echo -e "${GRAY}  网络模式: host（直接使用宿主机网络）${NC}"
echo -e "${GRAY}  推送到仓库: $PUSH_TO_REGISTRY${NC}"
if [ "$USE_LOCAL_FRONTEND" = "true" ]; then
    echo -e "${GRAY}  前端来源: 本地构建 ($FRONTEND_PROJECT_NAME)${NC}"
else
    echo -e "${GRAY}  前端来源: GitHub 自动下载${NC}"
fi
if [ "$USE_SYSTEM_CONFIG" = "true" ]; then
    echo -e "${GRAY}  配置来源: 系统配置 (config.yaml)${NC}"
else
    echo -e "${GRAY}  配置来源: 临时测试配置${NC}"
fi
echo ""

# 错误处理（模拟 try/catch/finally）
error_occurred=false
error_message=""

run_main() {
    # 计算总步骤数
    local total_steps=6
    if [ "$USE_LOCAL_FRONTEND" = "true" ]; then
        total_steps=7
    fi
    local current_step=0

    # ========================================
    # 步骤 1: 获取版本信息
    # ========================================
    current_step=$((current_step + 1))
    write_step "$current_step/$total_steps" "获取版本信息"

    pushd "$PROJECT_ROOT" > /dev/null

    write_command "git describe --tags --always --dirty" "读取 Git 版本标签"
    VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")

    write_command "git rev-parse --short HEAD" "读取 Git 提交哈希"
    COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "none")

    BUILD_DATE=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

    echo ""
    echo -e "${GREEN}版本信息:${NC}"
    echo -e "${GRAY}  Version: $VERSION${NC}"
    echo -e "${GRAY}  Commit: $COMMIT${NC}"
    echo -e "${GRAY}  Build Date: $BUILD_DATE${NC}"

    popd > /dev/null

    # ========================================
    # 步骤 2: 构建前端项目（可选）
    # ========================================
    if [ "$USE_LOCAL_FRONTEND" = "true" ]; then
        current_step=$((current_step + 1))
        write_step "$current_step/$total_steps" "构建前端项目"

        # 检查前端目录
        if [ ! -d "$FRONTEND_DIR" ]; then
            echo -e "${RED}错误: 找不到前端项目目录: $FRONTEND_DIR${NC}"
            echo -e "${YELLOW}提示: 请检查 FRONTEND_PROJECT_NAME 配置是否正确（当前值: $FRONTEND_PROJECT_NAME）${NC}"
            echo -e "${YELLOW}      或设置 USE_LOCAL_FRONTEND=false 使用 GitHub 下载${NC}"
            return 1
        fi

        pushd "$FRONTEND_DIR" > /dev/null

        # 检查并安装依赖
        if [ ! -d "node_modules" ]; then
            write_command "npm install" "安装前端依赖（可能需要几分钟）"
            npm install
            write_success "前端依赖安装完成"
        else
            write_success "前端依赖已存在"
        fi

        # 清除旧构建产物，避免构建失败时误用旧文件
        rm -rf dist

        # 构建前端
        write_command "npm run build" "构建前端项目"
        if ! npm run build; then
            popd > /dev/null
            echo -e "${RED}✗ 前端构建失败，请检查上方错误信息${NC}"
            return 1
        fi

        # 检查构建输出
        if [ ! -f "$FRONTEND_DIST_HTML" ]; then
            popd > /dev/null
            echo -e "${RED}构建输出文件不存在: $FRONTEND_DIST_HTML${NC}"
            return 1
        fi
        local file_size
        file_size=$(du -h "$FRONTEND_DIST_HTML" | cut -f1)
        echo -e "${GREEN}✓ 前端构建成功 (文件大小: ${file_size})${NC}"

        popd > /dev/null

        # 复制 HTML 到后端 static 目录
        echo ""
        echo -e "${YELLOW}复制前端文件到后端目录...${NC}"
        mkdir -p "$BACKEND_STATIC_DIR"
        write_command "cp \"$FRONTEND_DIST_HTML\" -> \"$BACKEND_MANAGEMENT_HTML\"" "复制前端构建文件"
        cp "$FRONTEND_DIST_HTML" "$BACKEND_MANAGEMENT_HTML"
        echo -e "${GREEN}✓ 前端文件已复制到: $BACKEND_MANAGEMENT_HTML${NC}"

        # 创建临时 Dockerfile（包含前端文件和环境变量）
        echo ""
        echo -e "${YELLOW}创建临时 Dockerfile...${NC}"
        cat > "$LOCAL_DOCKERFILE" << 'DOCKERFILE_EOF'
FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

ENV GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

FROM alpine:3.22.0

RUN apk add --no-cache tzdata

RUN mkdir -p /CLIProxyAPI/static

COPY --from=builder ./app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

# 复制本地构建的前端文件
COPY static/management.html /CLIProxyAPI/static/management.html

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai
# 设置环境变量禁止从 GitHub 下载前端
ENV MANAGEMENT_STATIC_PATH=/CLIProxyAPI/static/management.html

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
DOCKERFILE_EOF
        echo -e "${GREEN}✓ 临时 Dockerfile 已创建: $LOCAL_DOCKERFILE${NC}"
    fi

    # ========================================
    # 步骤 3: 构建 Docker 镜像
    # ========================================
    current_step=$((current_step + 1))
    write_step "$current_step/$total_steps" "构建 Docker 镜像"

    pushd "$PROJECT_ROOT" > /dev/null

    if [ "$USE_LOCAL_FRONTEND" = "true" ]; then
        local build_cmd="docker build --network=host -t $FULL_IMAGE_NAME -f Dockerfile.local --build-arg VERSION=$VERSION --build-arg COMMIT=$COMMIT --build-arg BUILD_DATE=$BUILD_DATE ."
        write_command "$build_cmd" "构建 Docker 镜像（使用本地前端，约 1-5 分钟）"

        if ! docker build --network=host -t "$FULL_IMAGE_NAME" \
            -f Dockerfile.local \
            --build-arg "VERSION=$VERSION" \
            --build-arg "COMMIT=$COMMIT" \
            --build-arg "BUILD_DATE=$BUILD_DATE" \
            .; then
            echo -e "${RED}✗ Docker 镜像构建失败${NC}"
            popd > /dev/null
            return 1
        fi
    else
        local build_cmd="docker build --network=host -t $FULL_IMAGE_NAME --build-arg VERSION=$VERSION --build-arg COMMIT=$COMMIT --build-arg BUILD_DATE=$BUILD_DATE ."
        write_command "$build_cmd" "构建 Docker 镜像（约 1-5 分钟，占用磁盘 50-100 MB）"

        if ! docker build --network=host -t "$FULL_IMAGE_NAME" \
            --build-arg "VERSION=$VERSION" \
            --build-arg "COMMIT=$COMMIT" \
            --build-arg "BUILD_DATE=$BUILD_DATE" \
            .; then
            echo -e "${RED}✗ Docker 镜像构建失败${NC}"
            popd > /dev/null
            return 1
        fi
    fi

    echo -e "${GREEN}✓ Docker 镜像构建成功: $FULL_IMAGE_NAME${NC}"

    popd > /dev/null

    # ========================================
    # 步骤 4: 准备测试环境
    # ========================================
    current_step=$((current_step + 1))
    write_step "$current_step/$total_steps" "准备测试环境"

    # 确定使用的配置文件
    local system_config_file="$PROJECT_ROOT/config.yaml"

    # 统一使用测试密钥（方便登录管理页面）
    local management_key="$TEST_MANAGEMENT_KEY"

    local container_port
    local api_key_for_health_check
    local config_file_to_mount
    local auth_dir_to_mount
    local log_dir_to_mount

    if [ "$USE_SYSTEM_CONFIG" = "true" ]; then
        # ========================================
        # 使用系统配置：挂载真实目录
        # ========================================
        if [ ! -f "$system_config_file" ]; then
            echo -e "${RED}错误: 系统配置文件不存在: $system_config_file${NC}"
            echo -e "${YELLOW}提示: 请先创建 config.yaml，或设置 USE_SYSTEM_CONFIG=false 使用临时测试配置${NC}"
            return 1
        fi

        # 从配置文件读取端口
        container_port=$(grep -E '^port:\s*[0-9]+' "$system_config_file" | head -1 | sed 's/^port:\s*//' | tr -d '[:space:]')
        if [ -z "$container_port" ]; then
            container_port=8317  # 默认端口
        fi

        # 从配置文件读取第一个 API Key（用于健康检查）
        api_key_for_health_check=$(grep -A1 '^api-keys:' "$system_config_file" | tail -1 | sed 's/^\s*-\s*//' | sed 's/^["'"'"']//' | sed 's/["'"'"']$//' | tr -d '[:space:]')
        if [ -z "$api_key_for_health_check" ]; then
            api_key_for_health_check="$TEST_API_KEY"  # 回退到测试 key
            echo -e "  ${YELLOW}⚠ 未找到 api-keys，健康检查将使用测试 key${NC}"
        fi

        echo -e "${GREEN}使用系统配置（挂载真实目录）:${NC}"
        write_filepath "源配置:" "$system_config_file"
        write_filepath "测试配置:" "$TEST_CONFIG_FILE" "(将创建)"

        # 复制系统配置文件，只修改必要的参数
        write_command "复制并修改配置文件" "复制 config.yaml -> config.test.yaml，修改 auth-dir/secret-key/allow-remote"

        local config_content
        config_content=$(<"$system_config_file")

        # 替换 auth-dir 为容器内路径（因为要挂载真实目录到容器内）
        config_content=$(echo "$config_content" | sed 's|^auth-dir:.*|auth-dir: "/root/.cli-proxy-api"|')

        # 替换 secret-key 为测试密钥（方便登录）
        config_content=$(echo "$config_content" | sed "s|^\(\s*\)secret-key:.*|\1secret-key: \"$management_key\"|")

        # 确保 allow-remote 为 true
        config_content=$(echo "$config_content" | sed 's|^\(\s*\)allow-remote:.*|\1allow-remote: true|')

        # 保存修改后的配置
        echo "$config_content" > "$TEST_CONFIG_FILE"

        write_success "测试配置已创建"
        echo -e "  ${GRAY}已修改: auth-dir=/root/.cli-proxy-api, secret-key=$management_key, allow-remote=true${NC}"

        config_file_to_mount="$TEST_CONFIG_FILE"
        auth_dir_to_mount="$SYSTEM_AUTH_DIR"   # 挂载真实认证目录
        log_dir_to_mount="$SYSTEM_LOG_DIR"     # 挂载真实日志目录

        # 确保系统目录存在
        if [ ! -d "$SYSTEM_AUTH_DIR" ]; then
            write_command "mkdir -p \"$SYSTEM_AUTH_DIR\"" "创建认证目录"
            mkdir -p "$SYSTEM_AUTH_DIR"
        fi
        if [ ! -d "$SYSTEM_LOG_DIR" ]; then
            write_command "mkdir -p \"$SYSTEM_LOG_DIR\"" "创建日志目录"
            mkdir -p "$SYSTEM_LOG_DIR"
        fi

        echo ""
        echo -e "${CYAN}配置摘要:${NC}"
        write_info "容器端口:" "$container_port (host 网络，直接监听)"
        write_filepath "认证目录:" "$SYSTEM_AUTH_DIR" "(真实目录)"
        write_filepath "日志目录:" "$SYSTEM_LOG_DIR" "(真实目录)"
        write_info "管理密钥:" "$management_key"
        write_info "健康检查 Key:" "$api_key_for_health_check"

    else
        # ========================================
        # 不使用系统配置：使用临时目录
        # ========================================
        container_port=8317
        api_key_for_health_check="$TEST_API_KEY"  # 使用测试 API Key

        # 创建最小化测试配置
        write_command "创建测试配置文件" "创建 config.test.yaml（测试后自动删除）"
        write_filepath "目标文件:" "$TEST_CONFIG_FILE" "(将创建)"
        cat > "$TEST_CONFIG_FILE" << EOF
host: ""
port: $container_port
remote-management:
  allow-remote: true
  secret-key: "$management_key"
auth-dir: "/root/.cli-proxy-api"
api-keys:
  - "$TEST_API_KEY"
debug: true
EOF
        write_success "测试配置文件已创建（最小化配置）"

        config_file_to_mount="$TEST_CONFIG_FILE"

        # 创建临时目录
        write_command "mkdir -p \"$TEST_AUTH_DIR\"" "创建临时认证目录（测试后删除）"
        mkdir -p "$TEST_AUTH_DIR"

        write_command "mkdir -p \"$TEST_LOG_DIR\"" "创建临时日志目录（测试后删除）"
        mkdir -p "$TEST_LOG_DIR"

        write_success "测试目录已创建"

        auth_dir_to_mount="$TEST_AUTH_DIR"     # 挂载临时目录
        log_dir_to_mount="$TEST_LOG_DIR"       # 挂载临时目录

        echo ""
        echo -e "${CYAN}配置摘要:${NC}"
        write_info "容器端口:" "$container_port (host 网络，直接监听)"
        write_filepath "认证目录:" "$TEST_AUTH_DIR" "(临时目录)"
        write_filepath "日志目录:" "$TEST_LOG_DIR" "(临时目录)"
        write_info "管理密钥:" "$management_key"
        write_info "健康检查 Key:" "$api_key_for_health_check"
    fi

    # ========================================
    # 步骤 5: 运行测试容器
    # ========================================
    current_step=$((current_step + 1))
    write_step "$current_step/$total_steps" "运行测试容器"

    # 先清理可能存在的旧容器
    local existing_container
    existing_container=$(docker ps -aq -f "name=$TEST_CONTAINER_NAME" 2>/dev/null || true)
    if [ -n "$existing_container" ]; then
        write_command "docker rm -f $TEST_CONTAINER_NAME" "删除已存在的同名容器"
        docker rm -f "$TEST_CONTAINER_NAME" 2>/dev/null || true
    fi

    # 根据配置决定描述信息
    local config_desc
    local frontend_desc
    if [ "$USE_SYSTEM_CONFIG" = "true" ]; then
        config_desc="基于系统配置"
    else
        config_desc="最小化配置"
    fi
    if [ "$USE_LOCAL_FRONTEND" = "true" ]; then
        frontend_desc="前端已内置"
    else
        frontend_desc="GitHub 前端"
    fi

    local run_cmd="docker run -d --name $TEST_CONTAINER_NAME --network=host -v \"${config_file_to_mount}:/CLIProxyAPI/config.yaml\" -v \"${auth_dir_to_mount}:/root/.cli-proxy-api\" -v \"${log_dir_to_mount}:/CLIProxyAPI/logs\" $FULL_IMAGE_NAME"
    write_command "$run_cmd" "启动测试容器（host 网络，端口 $container_port，$config_desc，$frontend_desc）"

    docker run -d \
        --name "$TEST_CONTAINER_NAME" \
        --network=host \
        -v "${config_file_to_mount}:/CLIProxyAPI/config.yaml" \
        -v "${auth_dir_to_mount}:/root/.cli-proxy-api" \
        -v "${log_dir_to_mount}:/CLIProxyAPI/logs" \
        "$FULL_IMAGE_NAME"

    if [ "$USE_LOCAL_FRONTEND" = "true" ]; then
        echo -e "  ${GRAY}镜像已内置前端文件和 MANAGEMENT_STATIC_PATH 环境变量${NC}"
    fi
    echo -e "  ${GRAY}配置来源: $config_desc${NC}"
    echo -e "  ${GRAY}管理密钥: $management_key${NC}"

    echo -e "${GREEN}✓ 测试容器已启动${NC}"

    # 标记容器已启动（用于 Ctrl+C 清理判断）
    CONTAINER_STARTED=true

    # ========================================
    # 交互式暂停：允许手动测试
    # ========================================

    system_config_file="$PROJECT_ROOT/config.yaml"
    local skip_health_check=false

    while true; do
        echo ""
        echo -e "${YELLOW}╔════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${YELLOW}║  容器已启动，可进行手动测试                                ║${NC}"
        echo -e "${YELLOW}╚════════════════════════════════════════════════════════════╝${NC}"
        echo ""
        echo -e "${CYAN}可用命令（在新终端中执行）:${NC}"
        echo -e "  ${GRAY}浏览器访问: ${GREEN}http://localhost:$container_port/management.html${NC}"
        echo -e "  ${GRAY}请求详情:   ${GREEN}http://localhost:$container_port/management.html#/detailed-requests${NC}"
        echo -e "  ${GRAY}查看日志:   ${CYAN}docker logs -f $TEST_CONTAINER_NAME${NC}"
        echo -e "  ${GRAY}进入容器:   ${CYAN}docker exec -it $TEST_CONTAINER_NAME sh${NC}"
        echo -e "  ${GRAY}API 测试:   ${CYAN}curl http://localhost:$container_port/v1/models -H \"Authorization: Bearer $api_key_for_health_check\"${NC}"
        echo ""
        echo -e "  ${GRAY}管理密钥:   ${YELLOW}$management_key${NC}"
        echo ""
        echo -e "${YELLOW}请选择操作:${NC}"
        echo -e "  ${GRAY}[Enter] 继续自动健康检查${NC}"
        echo -e "  ${GRAY}[d]     同步配置（config.test.yaml -> config.yaml，排除服务器配置）${NC}"
        echo -e "  ${GRAY}[s]     跳过健康检查，直接到推送步骤${NC}"
        echo -e "  ${GRAY}[q]     退出脚本并清理容器${NC}"
        echo -e "  ${GRAY}[n]     退出脚本（容器保持运行，需手动清理）${NC}"
        echo ""

        read -r -p "请输入选择: " action

        if [ "$action" = "d" ] || [ "$action" = "D" ]; then
            echo ""
            echo -e "${CYAN}同步配置...${NC}"
            sync_config_excluding_server_config "$TEST_CONFIG_FILE" "$system_config_file"
            # 继续循环，回到选择界面
            continue
        fi

        if [ "$action" = "q" ] || [ "$action" = "Q" ]; then
            echo ""
            echo -e "${YELLOW}用户选择退出并清理...${NC}"
            cleanup_test_resources
            echo -e "${GREEN}已退出${NC}"
            exit 0
        fi

        if [ "$action" = "n" ] || [ "$action" = "N" ]; then
            echo ""
            echo -e "${YELLOW}用户选择退出，容器保持运行。${NC}"
            echo -e "${CYAN}清理命令: docker stop $TEST_CONTAINER_NAME && docker rm $TEST_CONTAINER_NAME${NC}"
            echo ""
            # 不触发清理，直接退出
            exit 0
        fi

        # Enter 或 s 跳出循环，继续后续步骤
        if [ "$action" = "s" ] || [ "$action" = "S" ]; then
            skip_health_check=true
        fi
        break
    done

    # ========================================
    # 步骤 6: 健康检查
    # ========================================
    current_step=$((current_step + 1))
    write_step "$current_step/$total_steps" "健康检查"

    if [ "$skip_health_check" = "true" ]; then
        echo -e "${YELLOW}用户选择跳过健康检查${NC}"
    else
        local health_check_url="http://localhost:$container_port/management.html"
        local api_check_url="http://localhost:$container_port/v1/models"

        echo -e "${YELLOW}等待服务启动...${NC}"
        echo -e "  ${GRAY}管理页面: $health_check_url${NC}"
        echo -e "  ${GRAY}API 端点: $api_check_url${NC}"
        echo ""

        local start_time
        start_time=$(date +%s)
        local healthy=false
        local last_error=""

        while true; do
            local now
            now=$(date +%s)
            local elapsed=$((now - start_time))
            if [ "$elapsed" -ge "$HEALTH_CHECK_TIMEOUT" ]; then
                break
            fi

            sleep "$HEALTH_CHECK_INTERVAL"

            # 检查容器是否还在运行
            local container_status
            container_status=$(docker inspect -f '{{.State.Running}}' "$TEST_CONTAINER_NAME" 2>/dev/null || echo "false")
            if [ "$container_status" != "true" ]; then
                echo -e "${YELLOW}容器日志:${NC}"
                docker logs "$TEST_CONTAINER_NAME" 2>&1 | tail -20
                echo -e "${RED}容器已停止运行${NC}"
                return 1
            fi

            # 检查 API 端点
            local http_code
            http_code=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $api_key_for_health_check" --connect-timeout 5 "$api_check_url" 2>/dev/null || echo "000")

            if [ "$http_code" = "200" ]; then
                healthy=true
                break
            fi
            last_error="HTTP $http_code"
            elapsed=$(($(date +%s) - start_time))
            echo -e "  ${GRAY}等待中... (${elapsed}s / ${HEALTH_CHECK_TIMEOUT}s) - $last_error${NC}"
        done

        if [ "$healthy" != "true" ]; then
            echo ""
            echo -e "${YELLOW}容器日志（最后 30 行）:${NC}"
            docker logs "$TEST_CONTAINER_NAME" 2>&1 | tail -30
            echo -e "${RED}健康检查超时: $last_error${NC}"
            return 1
        fi

        echo ""
        echo -e "${GREEN}✓ API 端点响应正常 (HTTP 200)${NC}"

        # 检查管理页面
        local mgmt_code
        mgmt_code=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 "$health_check_url" 2>/dev/null || echo "000")
        if [ "$mgmt_code" = "200" ]; then
            echo -e "${GREEN}✓ 管理页面响应正常 (HTTP 200)${NC}"
        else
            echo -e "${YELLOW}⚠ 管理页面检查失败: HTTP $mgmt_code${NC}"
            echo -e "  ${GRAY}这可能是正常的（如果禁用了管理面板）${NC}"
        fi

        # 显示版本信息
        local version_url="http://localhost:$container_port/version"
        local version_response
        version_response=$(curl -s --connect-timeout 5 "$version_url" 2>/dev/null || true)
        if [ -n "$version_response" ]; then
            echo ""
            echo -e "${GREEN}版本端点响应:${NC}"
            echo -e "${GRAY}$version_response${NC}"
        fi

        echo ""
        echo -e "${GREEN}╔════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${GREEN}║  ✓ 所有健康检查通过！                                      ║${NC}"
        echo -e "${GREEN}╚════════════════════════════════════════════════════════════╝${NC}"
    fi

    # ========================================
    # 步骤 7: 推送到远程仓库（可选）
    # ========================================
    current_step=$((current_step + 1))
    write_step "$current_step/$total_steps" "推送到远程仓库"

    if [ "$PUSH_TO_REGISTRY" = "true" ]; then
        echo ""
        echo -e "${YELLOW}准备推送镜像到: $FULL_IMAGE_NAME${NC}"
        echo -e "${YELLOW}请确保已执行 'docker login' 登录到目标仓库${NC}"
        echo ""

        read -r -p "是否继续推送？(y/N) " confirm
        if [ "$confirm" = "y" ] || [ "$confirm" = "Y" ]; then
            write_command "docker push $FULL_IMAGE_NAME" "推送镜像到远程仓库（需要网络，可能需要几分钟）"
            docker push "$FULL_IMAGE_NAME"

            echo -e "${GREEN}✓ 镜像已推送到: $FULL_IMAGE_NAME${NC}"
        else
            echo -e "${YELLOW}已跳过推送${NC}"
        fi
    else
        echo -e "${YELLOW}已配置跳过推送（PUSH_TO_REGISTRY=false）${NC}"
    fi

    # ========================================
    # 完成
    # ========================================
    echo ""
    echo -e "${MAGENTA}╔════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${MAGENTA}║  ✓ 全部完成！                                              ║${NC}"
    echo -e "${MAGENTA}╚════════════════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${GRAY}镜像信息:${NC}"
    echo -e "${GRAY}  名称: $FULL_IMAGE_NAME${NC}"
    echo -e "${GRAY}  版本: $VERSION${NC}"
    echo -e "${GRAY}  提交: $COMMIT${NC}"
    echo ""
}

# 执行主流程（模拟 try/catch/finally）
if run_main; then
    : # 成功
else
    echo ""
    echo -e "${RED}╔════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${RED}║  ✗ 发生错误                                                ║${NC}"
    echo -e "${RED}╚════════════════════════════════════════════════════════════╝${NC}"
    echo ""
fi

# finally: 清理测试资源
cleanup_test_resources

# 可选：清理本地镜像
if [ "$CLEANUP_LOCAL_IMAGE" = "true" ]; then
    echo ""
    write_command "docker rmi $FULL_IMAGE_NAME" "删除本地测试镜像"
    docker rmi "$FULL_IMAGE_NAME" 2>/dev/null || true
    echo -e "${GREEN}✓ 本地镜像已删除${NC}"
fi
