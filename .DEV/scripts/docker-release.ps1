# docker-release.ps1 - Docker 构建、测试、发布一体化脚本
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
#    - 镜像名: 由 $ImageName 和 $ImageTag 配置决定
#    - 清理: 可选（由 $CleanupLocalImage 配置决定）
#
# 4. 运行 Docker 容器
#    - 影响: 启动临时容器，占用端口 $TestPort（默认 18317）
#    - 容器名: cli-proxy-api-test
#    - 清理: 测试后自动停止并删除
#
# 5. 构建前端项目（可选，当 $UseLocalFrontend = $true 时）
#    - 影响: 在前端目录执行 npm install 和 npm run build
#    - 位置: Cli-Proxy-API-Management-Center/dist/index.html
#    - 说明: 构建的 HTML 会挂载到容器中，并设置 MANAGEMENT_STATIC_PATH 环境变量禁止自动更新
#
# 6. 推送到远程仓库（可选）
#    - 影响: 上传镜像到 Docker Hub 或其他仓库（需要提前 docker login）
#    - 需要: 网络连接和仓库写入权限
#

<# 
使用说明
保存脚本：将上面内容保存到 CLIProxyAPI/_ai_scripts/docker-release.ps1
任意位置运行脚本即可
配置变量（脚本开头可修改）：
$ImageName - 镜像名称（默认 yuan019/cli-proxy-api）
$ImageTag - 镜像标签（默认 latest）
$PushToRegistry - 是否推送（默认 $true）
$TestPort - 测试端口（默认 18317）
$UseLocalFrontend - 是否使用本地前端（默认 $true）
  - $true: 构建本地前端项目，挂载到容器中，禁止从 GitHub 下载
  - $false: 使用 GitHub 自动下载的前端
推送前准备：
   docker login  # 登录 Docker Hub #>

# ========================================

$ErrorActionPreference = "Stop"

# ========================================
# 可配置变量（根据需要修改）
# ========================================

# Docker 镜像配置
$ImageName = "yuan019/cli-proxy-api"           # 镜像名称（包含仓库前缀）
$ImageTag = "latest"                           # 镜像标签
$PushToRegistry = $true                        # 是否推送到远程仓库
$CleanupLocalImage = $false                    # 测试完成后是否删除本地镜像

# 前端配置
$UseLocalFrontend = $true                      # $true: 使用本地构建的前端（默认）; $false: 使用 GitHub 下载
$FrontendProjectName = "Cli-Proxy-API-Management-Center"  # 前端项目目录名

# 测试配置
$TestPort = 18317                              # 测试用端口（避免与正在运行的服务冲突）
$TestContainerName = "cli-proxy-api-test"      # 测试容器名称
$HealthCheckTimeout = 60                       # 健康检查超时时间（秒）
$HealthCheckInterval = 2                       # 健康检查间隔（秒）
# 配置来源选项:
#   $true:  使用系统配置 - 挂载真实目录（可以看到真实的认证文件）
#           配置基于 config.yaml，继承 providers、api-keys 等设置
#           挂载: 真实认证目录 + 真实日志目录
#   $false: 使用测试配置 - 挂载临时目录（测试后自动清理）
#           配置为最小化设置，仅包含基本选项
#           挂载: 临时认证目录 + 临时日志目录
# 注意: 两种模式都会修改 auth-dir/secret-key 为测试值，方便容器内访问和登录
$UseSystemConfig = $true

# 测试配置参数
$TestApiKey = "test-api-key-for-docker-build"   # 仅 $UseSystemConfig=$false 时使用
$TestManagementKey = "test-management-key"      # 两种模式都使用（替换原密钥方便登录）

# 系统目录路径（当 $UseSystemConfig = $true 时挂载真实目录）
# 修改这里以匹配你的系统配置
$SystemAuthDirPath = "C:\Users\myuan\.cli-proxy-api"  # 与 config.yaml 中的 auth-dir 对应

# 项目目录名（用于定位项目根目录）
$ProjectDirName = "CLIProxyAPI"

# ========================================
# 自动计算路径（不需要修改）
# ========================================
$ScriptPath = $MyInvocation.MyCommand.Path
if ($ScriptPath) {
    $ScriptDir = Split-Path -Parent $ScriptPath
    $ProjectRoot = Split-Path -Parent (Split-Path -Parent $ScriptDir)  # 从 .dev/scripts/ 往上两层到项目根目录
} else {
    # 如果直接粘贴执行，尝试查找项目目录
    $ProjectRoot = Get-Location
    while ($ProjectRoot -and (Split-Path -Leaf $ProjectRoot) -ne $ProjectDirName) {
        $parent = Split-Path -Parent $ProjectRoot
        if ($parent -eq $ProjectRoot) {
            # 到达根目录
            break
        }
        $ProjectRoot = $parent
    }
    if ((Split-Path -Leaf $ProjectRoot) -ne $ProjectDirName) {
        # 尝试在当前目录下查找
        $candidates = @(
            (Join-Path (Get-Location) $ProjectDirName),
            (Join-Path (Split-Path -Parent (Get-Location)) $ProjectDirName)
        )
        foreach ($candidate in $candidates) {
            if (Test-Path $candidate) {
                $ProjectRoot = $candidate
                break
            }
        }
    }
}

# 验证项目根目录
$Dockerfile = Join-Path $ProjectRoot "Dockerfile"
if (-not (Test-Path $Dockerfile)) {
    Write-Host "错误: 无法找到项目根目录。请在 CLIProxyAPI 目录或其子目录中运行此脚本。" -ForegroundColor Red
    Write-Host "当前检测路径: $ProjectRoot" -ForegroundColor Yellow
    exit 1
}

# 前端项目路径（用于本地前端构建）
$WorkspaceRoot = Split-Path -Parent $ProjectRoot
$FrontendDir = Join-Path $WorkspaceRoot $FrontendProjectName
$FrontendDistHtml = Join-Path $FrontendDir "dist" "index.html"
$ContainerHtmlPath = "/CLIProxyAPI/static/management.html"

# 临时文件路径（仅当 $UseSystemConfig = $false 时使用）
$TestConfigFile = Join-Path $ProjectRoot "config.test.yaml"
$TestAuthDir = Join-Path $ProjectRoot "auths-test"
$TestLogDir = Join-Path $ProjectRoot "logs-test"
$LocalDockerfile = Join-Path $ProjectRoot "Dockerfile.local"
$BackendStaticDir = Join-Path $ProjectRoot "static"
$BackendManagementHtml = Join-Path $BackendStaticDir "management.html"

# 系统目录路径（使用顶部配置的变量）
$SystemAuthDir = $SystemAuthDirPath
$SystemLogDir = Join-Path $ProjectRoot "logs"

# 完整镜像名称
$FullImageName = "${ImageName}:${ImageTag}"

# ========================================
# 辅助函数 - 日志输出（使用彩色区分不同对象）
# ========================================
# 颜色规范:
#   Cyan    - 命令/步骤标题
#   Yellow  - 文件路径/目录
#   Green   - 成功消息
#   Red     - 错误消息
#   Magenta - 警告/重要提示
#   DarkGray - 影响描述/次要信息
#   White   - 普通文本

function Write-Command {
    param(
        [string]$Command,
        [string]$Description = ""
    )
    Write-Host ""
    Write-Host "> " -NoNewline -ForegroundColor Blue
    Write-Host $Command -ForegroundColor Cyan
    if ($Description) {
        Write-Host "  影响: " -NoNewline -ForegroundColor DarkGray
        Write-Host $Description -ForegroundColor DarkGray
    }
}

function Write-Step {
    param(
        [string]$StepNumber,
        [string]$StepName
    )
    Write-Host ""
    Write-Host "========================================" -ForegroundColor Cyan
    Write-Host "  [$StepNumber] $StepName" -ForegroundColor Cyan
    Write-Host "========================================" -ForegroundColor Cyan
}

function Write-FilePath {
    param(
        [string]$Label,
        [string]$Path,
        [string]$Extra = ""
    )
    Write-Host "  $Label " -NoNewline -ForegroundColor Gray
    Write-Host $Path -NoNewline -ForegroundColor Yellow
    if ($Extra) {
        Write-Host " $Extra" -ForegroundColor DarkGray
    } else {
        Write-Host ""
    }
}

function Write-Info {
    param(
        [string]$Label,
        [string]$Value
    )
    Write-Host "  $Label " -NoNewline -ForegroundColor Gray
    Write-Host $Value -ForegroundColor White
}

function Write-Success {
    param([string]$Message)
    Write-Host "✓ $Message" -ForegroundColor Green
}

function Write-Warning {
    param([string]$Message)
    Write-Host "⚠ $Message" -ForegroundColor Magenta
}

function Write-Error {
    param([string]$Message)
    Write-Host "✗ $Message" -ForegroundColor Red
}

function Cleanup-TestResources {
    Write-Host ""
    Write-Host "清理测试资源..." -ForegroundColor Yellow
    
    # 停止并删除测试容器
    $container = docker ps -aq -f "name=$TestContainerName" 2>$null
    if ($container) {
        Write-Command "docker stop $TestContainerName" "停止测试容器"
        docker stop $TestContainerName 2>$null | Out-Null
        Write-Command "docker rm $TestContainerName" "删除测试容器"
        docker rm $TestContainerName 2>$null | Out-Null
    }
    
    # 删除临时配置文件（两种模式都会创建）
    if (Test-Path $TestConfigFile) {
        Write-Command "Remove-Item `"$TestConfigFile`"" "删除临时配置文件"
        Remove-Item $TestConfigFile -Force
    }
    
    # 删除临时目录（仅当不使用系统配置时才创建，不删除系统真实目录）
    if (-not $UseSystemConfig) {
        if (Test-Path $TestAuthDir) {
            Write-Command "Remove-Item -Recurse `"$TestAuthDir`"" "删除临时认证目录"
            Remove-Item $TestAuthDir -Recurse -Force
        }
        if (Test-Path $TestLogDir) {
            Write-Command "Remove-Item -Recurse `"$TestLogDir`"" "删除临时日志目录"
            Remove-Item $TestLogDir -Recurse -Force
        }
    }
    
    # 删除临时 Dockerfile
    if (Test-Path $LocalDockerfile) {
        Write-Command "Remove-Item `"$LocalDockerfile`"" "删除临时 Dockerfile"
        Remove-Item $LocalDockerfile -Force
    }
    
    # 删除临时 static 目录（仅在使用本地前端时创建）
    if (Test-Path $BackendStaticDir) {
        Write-Command "Remove-Item -Recurse `"$BackendStaticDir`"" "删除临时 static 目录"
        Remove-Item $BackendStaticDir -Recurse -Force
    }
    
    Write-Success "测试资源已清理"
}

# 注册清理函数（确保脚本退出时清理）
$script:CleanupRegistered = $false
function Register-Cleanup {
    if (-not $script:CleanupRegistered) {
        $script:CleanupRegistered = $true
        # PowerShell 没有原生的 trap，使用 try/finally 替代
    }
}

# 同步配置文件（排除服务器配置字段）
function Sync-ConfigExcludingServerConfig {
    param(
        [string]$SourceFile,    # config.test.yaml
        [string]$TargetFile     # config.yaml
    )
    
    if (-not (Test-Path $SourceFile)) {
        Write-Host "  ✗ 源文件不存在: $SourceFile" -ForegroundColor Red
        return
    }
    if (-not (Test-Path $TargetFile)) {
        Write-Host "  ✗ 目标文件不存在: $TargetFile" -ForegroundColor Red
        return
    }
    
    # 读取源配置和目标配置
    $sourceContent = Get-Content $SourceFile -Raw
    $targetContent = Get-Content $TargetFile -Raw
    
    # 需要保留的字段及其正则表达式
    # 这些字段从目标文件保留，不从源文件同步
    $preservePatterns = @(
        # host 字段（包括前面的注释）
        @{
            Name = "host"
            Pattern = '(?m)^(#[^\r\n]*[\r\n]+)*host:\s*[^\r\n]*'
        },
        # port 字段（包括前面的注释）
        @{
            Name = "port"
            Pattern = '(?m)^(#[^\r\n]*[\r\n]+)*port:\s*\d+'
        },
        # tls 整个块（包括前面的注释和子字段）
        @{
            Name = "tls"
            Pattern = '(?m)^(#[^\r\n]*[\r\n]+)*tls:[\r\n]+(\s+[^\r\n]+[\r\n]+)*'
        },
        # remote-management 整个块（包括前面的注释和子字段）
        @{
            Name = "remote-management"
            Pattern = '(?m)^(#[^\r\n]*[\r\n]+)*remote-management:[\r\n]+((\s+[^\r\n]*|#[^\r\n]*)[\r\n]+)*'
        },
        # auth-dir 字段（包括前面的注释）
        @{
            Name = "auth-dir"
            Pattern = '(?m)^(#[^\r\n]*[\r\n]+)*auth-dir:\s*[^\r\n]+'
        }
    )
    
    $newContent = $sourceContent
    $preservedFields = @()
    
    foreach ($item in $preservePatterns) {
        $pattern = $item.Pattern
        $name = $item.Name
        
        # 从目标文件提取原值
        if ($targetContent -match $pattern) {
            $originalValue = $Matches[0]
            
            # 从源文件提取值（用于对比）
            if ($sourceContent -match $pattern) {
                $sourceValue = $Matches[0]
                
                # 替换源内容中的值为目标文件的原值
                if ($sourceValue -ne $originalValue) {
                    $newContent = $newContent -replace [regex]::Escape($sourceValue), $originalValue
                    $preservedFields += $name
                }
            }
        }
    }
    
    # 检查是否有实际变化
    if ($newContent -eq $targetContent) {
        Write-Host "  ✓ 无需同步（内容相同）" -ForegroundColor Gray
        return
    }
    
    # 写入目标文件
    $newContent | Out-File -FilePath $TargetFile -Encoding utf8 -NoNewline
    
    # 打印同步结果
    Write-Host "  ✓ 配置已同步" -ForegroundColor Green
    Write-Host "    $SourceFile -> $TargetFile" -ForegroundColor Gray
    
    # 显示保留的字段
    if ($preservedFields.Count -gt 0) {
        Write-Host "    已保留原值: $($preservedFields -join ', ')" -ForegroundColor Magenta
    }
}

# ========================================
# Ctrl+C 中断处理
# ========================================
# 标记容器是否已启动（用于判断是否需要清理）
$script:ContainerStarted = $false

trap {
    Write-Host ""
    Write-Host "检测到中断信号 (Ctrl+C)..." -ForegroundColor Yellow
    if ($script:ContainerStarted) {
        Cleanup-TestResources
    }
    exit 1
}

# ========================================
# 主流程
# ========================================

Write-Host ""
Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Magenta
Write-Host "║     Docker 构建、测试、发布一体化脚本                      ║" -ForegroundColor Magenta
Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Magenta
Write-Host ""
Write-Host "配置信息:" -ForegroundColor Gray
Write-Host "  项目根目录: $ProjectRoot" -ForegroundColor Gray
Write-Host "  镜像名称: $FullImageName" -ForegroundColor Gray
Write-Host "  测试端口: $TestPort" -ForegroundColor Gray
Write-Host "  推送到仓库: $PushToRegistry" -ForegroundColor Gray
if ($UseLocalFrontend) {
    Write-Host "  前端来源: 本地构建 ($FrontendProjectName)" -ForegroundColor Gray
} else {
    Write-Host "  前端来源: GitHub 自动下载" -ForegroundColor Gray
}
if ($UseSystemConfig) {
    Write-Host "  配置来源: 系统配置 (config.yaml)" -ForegroundColor Gray
} else {
    Write-Host "  配置来源: 临时测试配置" -ForegroundColor Gray
}
Write-Host ""

try {
    # 计算总步骤数
    $TotalSteps = if ($UseLocalFrontend) { 7 } else { 6 }
    $CurrentStep = 0

    # ========================================
    # 步骤 1: 获取版本信息
    # ========================================
    $CurrentStep++
    Write-Step "$CurrentStep/$TotalSteps" "获取版本信息"
    
    Push-Location $ProjectRoot
    try {
        Write-Command "git describe --tags --always --dirty" "读取 Git 版本标签"
        $VERSION = git describe --tags --always --dirty 2>$null
        if (-not $VERSION) { $VERSION = "dev" }
        
        Write-Command "git rev-parse --short HEAD" "读取 Git 提交哈希"
        $COMMIT = git rev-parse --short HEAD 2>$null
        if (-not $COMMIT) { $COMMIT = "none" }
        
        $BUILD_DATE = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
        
        Write-Host ""
        Write-Host "版本信息:" -ForegroundColor Green
        Write-Host "  Version: $VERSION" -ForegroundColor Gray
        Write-Host "  Commit: $COMMIT" -ForegroundColor Gray
        Write-Host "  Build Date: $BUILD_DATE" -ForegroundColor Gray
    } finally {
        Pop-Location
    }

    # ========================================
    # 步骤 2: 构建前端项目（可选）
    # ========================================
    if ($UseLocalFrontend) {
        $CurrentStep++
        Write-Step "$CurrentStep/$TotalSteps" "构建前端项目"
        
        # 检查前端目录
        if (-not (Test-Path $FrontendDir)) {
            Write-Host "错误: 找不到前端项目目录: $FrontendDir" -ForegroundColor Red
            Write-Host "提示: 请检查 FrontendProjectName 配置是否正确（当前值: $FrontendProjectName）" -ForegroundColor Yellow
            Write-Host "      或设置 `$UseLocalFrontend = `$false 使用 GitHub 下载" -ForegroundColor Yellow
            throw "前端项目目录不存在"
        }
        
        Push-Location $FrontendDir
        try {
            # 检查并安装依赖
            if (-not (Test-Path "node_modules")) {
                Write-Command "npm install" "安装前端依赖（可能需要几分钟）"
                npm install
                if ($LASTEXITCODE -ne 0) {
                    throw "前端依赖安装失败"
                }
                Write-Success "前端依赖安装完成"
            } else {
                Write-Success "前端依赖已存在"
            }
            
            # 清除旧构建产物，避免构建失败时误用旧文件
            if (Test-Path "dist") { Remove-Item -Recurse -Force "dist" }

            # 构建前端
            Write-Command "npm run build" "构建前端项目"
            npm run build
            if ($LASTEXITCODE -ne 0) {
                throw "前端构建失败，请检查上方错误信息"
            }
            
            # 检查构建输出
            if (-not (Test-Path $FrontendDistHtml)) {
                throw "构建输出文件不存在: $FrontendDistHtml"
            }
            $fileSize = (Get-Item $FrontendDistHtml).Length
            Write-Host "✓ 前端构建成功 (文件大小: $([math]::Round($fileSize/1MB, 2)) MB)" -ForegroundColor Green
        } finally {
            Pop-Location
        }
        
        # 复制 HTML 到后端 static 目录
        Write-Host ""
        Write-Host "复制前端文件到后端目录..." -ForegroundColor Yellow
        if (-not (Test-Path $BackendStaticDir)) {
            New-Item -ItemType Directory -Path $BackendStaticDir -Force | Out-Null
        }
        Write-Command "Copy-Item `"$FrontendDistHtml`" -> `"$BackendManagementHtml`"" "复制前端构建文件"
        Copy-Item -Path $FrontendDistHtml -Destination $BackendManagementHtml -Force
        Write-Host "✓ 前端文件已复制到: $BackendManagementHtml" -ForegroundColor Green
        
        # 创建临时 Dockerfile（包含前端文件和环境变量）
        Write-Host ""
        Write-Host "创建临时 Dockerfile..." -ForegroundColor Yellow
        $dockerfileContent = @"
FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X 'main.Version=`${VERSION}' -X 'main.Commit=`${COMMIT}' -X 'main.BuildDate=`${BUILD_DATE}'" -o ./CLIProxyAPI ./cmd/server/

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

RUN cp /usr/share/zoneinfo/`${TZ} /etc/localtime && echo "`${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
"@
        $dockerfileContent | Out-File -FilePath $LocalDockerfile -Encoding utf8
        Write-Host "✓ 临时 Dockerfile 已创建: $LocalDockerfile" -ForegroundColor Green
    }

    # ========================================
    # 步骤 3: 构建 Docker 镜像
    # ========================================
    $CurrentStep++
    Write-Step "$CurrentStep/$TotalSteps" "构建 Docker 镜像"
    
    Push-Location $ProjectRoot
    try {
        if ($UseLocalFrontend) {
            $buildCmd = "docker build -t $FullImageName -f Dockerfile.local --build-arg VERSION=$VERSION --build-arg COMMIT=$COMMIT --build-arg BUILD_DATE=$BUILD_DATE ."
            Write-Command $buildCmd "构建 Docker 镜像（使用本地前端，约 1-5 分钟）"
            
            docker build -t $FullImageName `
                -f Dockerfile.local `
                --build-arg VERSION=$VERSION `
                --build-arg COMMIT=$COMMIT `
                --build-arg BUILD_DATE=$BUILD_DATE `
                .
        } else {
            $buildCmd = "docker build -t $FullImageName --build-arg VERSION=$VERSION --build-arg COMMIT=$COMMIT --build-arg BUILD_DATE=$BUILD_DATE ."
            Write-Command $buildCmd "构建 Docker 镜像（约 1-5 分钟，占用磁盘 50-100 MB）"
            
            docker build -t $FullImageName `
                --build-arg VERSION=$VERSION `
                --build-arg COMMIT=$COMMIT `
                --build-arg BUILD_DATE=$BUILD_DATE `
                .
        }
        
        if ($LASTEXITCODE -ne 0) {
            throw "Docker 镜像构建失败"
        }
        Write-Host "✓ Docker 镜像构建成功: $FullImageName" -ForegroundColor Green
    } finally {
        Pop-Location
    }

    # ========================================
    # 步骤 4: 准备测试环境
    # ========================================
    $CurrentStep++
    Write-Step "$CurrentStep/$TotalSteps" "准备测试环境"
    
    # 确定使用的配置文件
    $SystemConfigFile = Join-Path $ProjectRoot "config.yaml"
    
    # 统一使用测试密钥（方便登录管理页面）
    $ManagementKey = $TestManagementKey
    
    if ($UseSystemConfig) {
        # ========================================
        # 使用系统配置：挂载真实目录
        # ========================================
        if (-not (Test-Path $SystemConfigFile)) {
            Write-Host "错误: 系统配置文件不存在: $SystemConfigFile" -ForegroundColor Red
            Write-Host "提示: 请先创建 config.yaml，或设置 `$UseSystemConfig = `$false 使用临时测试配置" -ForegroundColor Yellow
            throw "系统配置文件不存在"
        }
        
        # 从配置文件读取端口
        $configContent = Get-Content $SystemConfigFile -Raw
        if ($configContent -match '(?m)^port:\s*(\d+)') {
            $ContainerPort = [int]$Matches[1]
        } else {
            $ContainerPort = 8317  # 默认端口
        }
        
        # 从配置文件读取第一个 API Key（用于健康检查）
        if ($configContent -match '(?m)^api-keys:\s*[\r\n]+\s*-\s*"([^"]+)"') {
            $ApiKeyForHealthCheck = $Matches[1]
        } elseif ($configContent -match "(?m)^api-keys:\s*[\r\n]+\s*-\s*'([^']+)'") {
            $ApiKeyForHealthCheck = $Matches[1]
        } elseif ($configContent -match '(?m)^api-keys:\s*[\r\n]+\s*-\s*(\S+)') {
            $ApiKeyForHealthCheck = $Matches[1]
        } else {
            $ApiKeyForHealthCheck = $TestApiKey  # 回退到测试 key
            Write-Host "  ⚠ 未找到 api-keys，健康检查将使用测试 key" -ForegroundColor Yellow
        }
        
        Write-Host "使用系统配置（挂载真实目录）:" -ForegroundColor Green
        Write-FilePath "源配置:" $SystemConfigFile
        Write-FilePath "测试配置:" $TestConfigFile "(将创建)"
        
        # 复制系统配置文件，只修改必要的参数
        Write-Command "复制并修改配置文件" "复制 config.yaml -> config.test.yaml，修改 auth-dir/secret-key/allow-remote"
        
        # 替换 auth-dir 为容器内路径（因为要挂载真实目录到容器内）
        $configContent = $configContent -replace '(?m)^auth-dir:\s*"[^"]*"', 'auth-dir: "/root/.cli-proxy-api"'
        $configContent = $configContent -replace "(?m)^auth-dir:\s*'[^']*'", 'auth-dir: "/root/.cli-proxy-api"'
        $configContent = $configContent -replace '(?m)^auth-dir:\s*[^\r\n]+', 'auth-dir: "/root/.cli-proxy-api"'
        
        # 替换 secret-key 为测试密钥（方便登录）
        $configContent = $configContent -replace '(?m)^(\s*)secret-key:\s*"[^"]*"', "`$1secret-key: `"$ManagementKey`""
        $configContent = $configContent -replace "(?m)^(\s*)secret-key:\s*'[^']*'", "`$1secret-key: `"$ManagementKey`""
        $configContent = $configContent -replace '(?m)^(\s*)secret-key:\s*\S[^\r\n]*', "`$1secret-key: `"$ManagementKey`""
        
        # 确保 allow-remote 为 true
        $configContent = $configContent -replace '(?m)^(\s*)allow-remote:\s*(true|false)', '$1allow-remote: true'
        
        # 保存修改后的配置
        $configContent | Out-File -FilePath $TestConfigFile -Encoding utf8
        
        Write-Success "测试配置已创建"
        Write-Host "  已修改: auth-dir=/root/.cli-proxy-api, secret-key=$ManagementKey, allow-remote=true" -ForegroundColor DarkGray
        
        $ConfigFileToMount = $TestConfigFile
        $AuthDirToMount = $SystemAuthDir   # 挂载真实认证目录
        $LogDirToMount = $SystemLogDir     # 挂载真实日志目录
        
        # 确保系统目录存在
        if (-not (Test-Path $SystemAuthDir)) {
            Write-Command "New-Item -ItemType Directory -Path `"$SystemAuthDir`"" "创建认证目录"
            New-Item -ItemType Directory -Path $SystemAuthDir -Force | Out-Null
        }
        if (-not (Test-Path $SystemLogDir)) {
            Write-Command "New-Item -ItemType Directory -Path `"$SystemLogDir`"" "创建日志目录"
            New-Item -ItemType Directory -Path $SystemLogDir -Force | Out-Null
        }
        
        Write-Host ""
        Write-Host "配置摘要:" -ForegroundColor Cyan
        Write-Info "容器端口:" "$ContainerPort (映射到外部 $TestPort)"
        Write-FilePath "认证目录:" $SystemAuthDir "(真实目录)"
        Write-FilePath "日志目录:" $SystemLogDir "(真实目录)"
        Write-Info "管理密钥:" $ManagementKey
        Write-Info "健康检查 Key:" $ApiKeyForHealthCheck
        
    } else {
        # ========================================
        # 不使用系统配置：使用临时目录
        # ========================================
        $ContainerPort = 8317
        $ApiKeyForHealthCheck = $TestApiKey  # 使用测试 API Key
        
        # 创建最小化测试配置
        Write-Command "创建测试配置文件" "创建 config.test.yaml（测试后自动删除）"
        Write-FilePath "目标文件:" $TestConfigFile "(将创建)"
        $testConfig = @"
host: ""
port: $ContainerPort
remote-management:
  allow-remote: true
  secret-key: "$ManagementKey"
auth-dir: "/root/.cli-proxy-api"
api-keys:
  - "$TestApiKey"
debug: true
"@
        $testConfig | Out-File -FilePath $TestConfigFile -Encoding utf8
        Write-Success "测试配置文件已创建（最小化配置）"
        
        $ConfigFileToMount = $TestConfigFile
        
        # 创建临时目录
        Write-Command "New-Item -ItemType Directory -Path `"$TestAuthDir`"" "创建临时认证目录（测试后删除）"
        New-Item -ItemType Directory -Path $TestAuthDir -Force | Out-Null
        
        Write-Command "New-Item -ItemType Directory -Path `"$TestLogDir`"" "创建临时日志目录（测试后删除）"
        New-Item -ItemType Directory -Path $TestLogDir -Force | Out-Null
        
        Write-Success "测试目录已创建"
        
        $AuthDirToMount = $TestAuthDir     # 挂载临时目录
        $LogDirToMount = $TestLogDir       # 挂载临时目录
        
        Write-Host ""
        Write-Host "配置摘要:" -ForegroundColor Cyan
        Write-Info "容器端口:" "$ContainerPort (映射到外部 $TestPort)"
        Write-FilePath "认证目录:" $TestAuthDir "(临时目录)"
        Write-FilePath "日志目录:" $TestLogDir "(临时目录)"
        Write-Info "管理密钥:" $ManagementKey
        Write-Info "健康检查 Key:" $ApiKeyForHealthCheck
    }

    # ========================================
    # 步骤 5: 运行测试容器
    # ========================================
    $CurrentStep++
    Write-Step "$CurrentStep/$TotalSteps" "运行测试容器"
    
    # 先清理可能存在的旧容器
    $existingContainer = docker ps -aq -f "name=$TestContainerName" 2>$null
    if ($existingContainer) {
        Write-Command "docker rm -f $TestContainerName" "删除已存在的同名容器"
        docker rm -f $TestContainerName 2>$null | Out-Null
    }
    
    # 根据配置决定描述信息
    $configDesc = if ($UseSystemConfig) { "基于系统配置" } else { "最小化配置" }
    $frontendDesc = if ($UseLocalFrontend) { "前端已内置" } else { "GitHub 前端" }
    
    $runCmd = "docker run -d --name $TestContainerName -p ${TestPort}:${ContainerPort} -v `"${ConfigFileToMount}:/CLIProxyAPI/config.yaml`" -v `"${AuthDirToMount}:/root/.cli-proxy-api`" -v `"${LogDirToMount}:/CLIProxyAPI/logs`" $FullImageName"
    Write-Command $runCmd "启动测试容器（端口 $TestPort，$configDesc，$frontendDesc）"
    
    docker run -d `
        --name $TestContainerName `
        -p "${TestPort}:${ContainerPort}" `
        -v "${ConfigFileToMount}:/CLIProxyAPI/config.yaml" `
        -v "${AuthDirToMount}:/root/.cli-proxy-api" `
        -v "${LogDirToMount}:/CLIProxyAPI/logs" `
        $FullImageName
    
    if ($UseLocalFrontend) {
        Write-Host "  镜像已内置前端文件和 MANAGEMENT_STATIC_PATH 环境变量" -ForegroundColor Gray
    }
    Write-Host "  配置来源: $configDesc" -ForegroundColor Gray
    Write-Host "  管理密钥: $ManagementKey" -ForegroundColor Gray
    
    if ($LASTEXITCODE -ne 0) {
        throw "测试容器启动失败"
    }
    Write-Host "✓ 测试容器已启动" -ForegroundColor Green
    
    # 标记容器已启动（用于 Ctrl+C 清理判断）
    $script:ContainerStarted = $true

    # ========================================
    # 交互式暂停：允许手动测试
    # ========================================
    
    $SystemConfigFile = Join-Path $ProjectRoot "config.yaml"
    
    while ($true) {
        Write-Host ""
        Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Yellow
        Write-Host "║  容器已启动，可进行手动测试                                ║" -ForegroundColor Yellow
        Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Yellow
        Write-Host ""
        Write-Host "可用命令（在新终端中执行）:" -ForegroundColor Cyan
        Write-Host "  浏览器访问: " -NoNewline -ForegroundColor Gray
        Write-Host "http://localhost:$TestPort/management.html" -ForegroundColor Green
        Write-Host "  请求详情:   " -NoNewline -ForegroundColor Gray
        Write-Host "http://localhost:$TestPort/management.html#/detailed-requests" -ForegroundColor Green
        Write-Host "  查看日志:   " -NoNewline -ForegroundColor Gray
        Write-Host "docker logs -f $TestContainerName" -ForegroundColor Cyan
        Write-Host "  进入容器:   " -NoNewline -ForegroundColor Gray
        Write-Host "docker exec -it $TestContainerName sh" -ForegroundColor Cyan
        Write-Host "  API 测试:   " -NoNewline -ForegroundColor Gray
        Write-Host "curl http://localhost:$TestPort/v1/models -H `"Authorization: Bearer $ApiKeyForHealthCheck`"" -ForegroundColor Cyan
        Write-Host ""
        Write-Host "  管理密钥:   " -NoNewline -ForegroundColor Gray
        Write-Host "$ManagementKey" -ForegroundColor Yellow
        Write-Host ""
        Write-Host "请选择操作:" -ForegroundColor Yellow
        Write-Host "  [Enter] 继续自动健康检查" -ForegroundColor Gray
        Write-Host "  [d]     同步配置（config.test.yaml -> config.yaml，排除服务器配置）" -ForegroundColor Gray
        Write-Host "  [s]     跳过健康检查，直接到推送步骤" -ForegroundColor Gray
        Write-Host "  [q]     退出脚本并清理容器" -ForegroundColor Gray
        Write-Host "  [n]     退出脚本（容器保持运行，需手动清理）" -ForegroundColor Gray
        Write-Host ""
        
        $action = Read-Host "请输入选择"
        
        if ($action -eq "d" -or $action -eq "D") {
            Write-Host ""
            Write-Host "同步配置..." -ForegroundColor Cyan
            Sync-ConfigExcludingServerConfig -SourceFile $TestConfigFile -TargetFile $SystemConfigFile
            # 继续循环，回到选择界面
            continue
        }
        
        if ($action -eq "q" -or $action -eq "Q") {
            Write-Host ""
            Write-Host "用户选择退出并清理..." -ForegroundColor Yellow
            Cleanup-TestResources
            Write-Host "已退出" -ForegroundColor Green
            exit 0
        }
        
        if ($action -eq "n" -or $action -eq "N") {
            Write-Host ""
            Write-Host "用户选择退出，容器保持运行。" -ForegroundColor Yellow
            Write-Host "清理命令: docker stop $TestContainerName && docker rm $TestContainerName" -ForegroundColor Cyan
            Write-Host ""
            # 不触发清理，直接退出
            exit 0
        }
        
        # Enter 或 s 跳出循环，继续后续步骤
        $SkipHealthCheck = ($action -eq "s" -or $action -eq "S")
        break
    }

    # ========================================
    # 步骤 6: 健康检查
    # ========================================
    $CurrentStep++
    Write-Step "$CurrentStep/$TotalSteps" "健康检查"
    
    if ($SkipHealthCheck) {
        Write-Host "用户选择跳过健康检查" -ForegroundColor Yellow
    } else {
        $healthCheckUrl = "http://localhost:$TestPort/management.html"
        $apiCheckUrl = "http://localhost:$TestPort/v1/models"
        
        Write-Host "等待服务启动..." -ForegroundColor Yellow
        Write-Host "  管理页面: $healthCheckUrl" -ForegroundColor Gray
        Write-Host "  API 端点: $apiCheckUrl" -ForegroundColor Gray
        Write-Host ""
        
        $startTime = Get-Date
        $healthy = $false
        $lastError = ""
        
        while (((Get-Date) - $startTime).TotalSeconds -lt $HealthCheckTimeout) {
            Start-Sleep -Seconds $HealthCheckInterval
            
            try {
                # 检查容器是否还在运行
                $containerStatus = docker inspect -f '{{.State.Running}}' $TestContainerName 2>$null
                if ($containerStatus -ne "true") {
                    Write-Host "容器日志:" -ForegroundColor Yellow
                    docker logs $TestContainerName 2>&1 | Select-Object -Last 20
                    throw "容器已停止运行"
                }
                
                # 检查 API 端点
                Write-Command "curl -s -o NUL -w `"%{http_code}`" $apiCheckUrl" "检查 API 端点响应"
                $response = Invoke-WebRequest -Uri $apiCheckUrl -Method GET -Headers @{"Authorization"="Bearer $ApiKeyForHealthCheck"} -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
                
                if ($response.StatusCode -eq 200) {
                    $healthy = $true
                    break
                }
                $lastError = "HTTP $($response.StatusCode)"
            } catch {
                $lastError = $_.Exception.Message
                $elapsed = [math]::Round(((Get-Date) - $startTime).TotalSeconds)
                Write-Host "  等待中... (${elapsed}s / ${HealthCheckTimeout}s) - $lastError" -ForegroundColor Gray
            }
        }
        
        if (-not $healthy) {
            Write-Host ""
            Write-Host "容器日志（最后 30 行）:" -ForegroundColor Yellow
            docker logs $TestContainerName 2>&1 | Select-Object -Last 30
            throw "健康检查超时: $lastError"
        }
        
        Write-Host ""
        Write-Host "✓ API 端点响应正常 (HTTP 200)" -ForegroundColor Green
        
        # 检查管理页面
        try {
            Write-Command "curl -s -o NUL -w `"%{http_code}`" $healthCheckUrl" "检查管理页面响应"
            $mgmtResponse = Invoke-WebRequest -Uri $healthCheckUrl -Method GET -TimeoutSec 5 -UseBasicParsing -ErrorAction Stop
            if ($mgmtResponse.StatusCode -eq 200) {
                Write-Host "✓ 管理页面响应正常 (HTTP 200)" -ForegroundColor Green
            }
        } catch {
            Write-Host "⚠ 管理页面检查失败: $($_.Exception.Message)" -ForegroundColor Yellow
            Write-Host "  这可能是正常的（如果禁用了管理面板）" -ForegroundColor Gray
        }
        
        # 显示版本信息
        try {
            $versionUrl = "http://localhost:$TestPort/version"
            $versionResponse = Invoke-WebRequest -Uri $versionUrl -Method GET -TimeoutSec 5 -UseBasicParsing -ErrorAction SilentlyContinue
            if ($versionResponse.StatusCode -eq 200) {
                Write-Host ""
                Write-Host "版本端点响应:" -ForegroundColor Green
                Write-Host $versionResponse.Content -ForegroundColor Gray
            }
        } catch {
            # 忽略版本端点错误
        }
        
        Write-Host ""
        Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Green
        Write-Host "║  ✓ 所有健康检查通过！                                      ║" -ForegroundColor Green
        Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Green
    }

    # ========================================
    # 步骤 7: 推送到远程仓库（可选）
    # ========================================
    $CurrentStep++
    Write-Step "$CurrentStep/$TotalSteps" "推送到远程仓库"
    
    if ($PushToRegistry) {
        Write-Host ""
        Write-Host "准备推送镜像到: $FullImageName" -ForegroundColor Yellow
        Write-Host "请确保已执行 'docker login' 登录到目标仓库" -ForegroundColor Yellow
        Write-Host ""
        
        $confirm = Read-Host "是否继续推送？(y/N)"
        if ($confirm -eq "y" -or $confirm -eq "Y") {
            Write-Command "docker push $FullImageName" "推送镜像到远程仓库（需要网络，可能需要几分钟）"
            docker push $FullImageName
            
            if ($LASTEXITCODE -ne 0) {
                throw "镜像推送失败"
            }
            Write-Host "✓ 镜像已推送到: $FullImageName" -ForegroundColor Green
        } else {
            Write-Host "已跳过推送" -ForegroundColor Yellow
        }
    } else {
        Write-Host "已配置跳过推送（`$PushToRegistry = `$false）" -ForegroundColor Yellow
    }

    # ========================================
    # 完成
    # ========================================
    Write-Host ""
    Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Magenta
    Write-Host "║  ✓ 全部完成！                                              ║" -ForegroundColor Magenta
    Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Magenta
    Write-Host ""
    Write-Host "镜像信息:" -ForegroundColor Gray
    Write-Host "  名称: $FullImageName" -ForegroundColor Gray
    Write-Host "  版本: $VERSION" -ForegroundColor Gray
    Write-Host "  提交: $COMMIT" -ForegroundColor Gray
    Write-Host ""

} catch {
    Write-Host ""
    Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Red
    Write-Host "║  ✗ 发生错误                                                ║" -ForegroundColor Red
    Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Red
    Write-Host ""
    Write-Host "错误信息: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host ""
} finally {
    # 清理测试资源
    Cleanup-TestResources
    
    # 可选：清理本地镜像
    if ($CleanupLocalImage) {
        Write-Host ""
        Write-Command "docker rmi $FullImageName" "删除本地测试镜像"
        docker rmi $FullImageName 2>$null | Out-Null
        Write-Host "✓ 本地镜像已删除" -ForegroundColor Green
    }
}