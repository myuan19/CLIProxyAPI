# docker-run.ps1 - Docker 拉取并运行脚本（使用本地配置）
#
# 功能：拉取 Docker 镜像 -> 使用本地配置运行容器
# 支持在任意位置执行
#
# ========================================
# 脚本影响说明
# ========================================
# 本脚本会执行以下操作，可能影响你的系统：
#
# 1. 拉取 Docker 镜像
#    - 影响: 下载镜像到本地 Docker（占用磁盘空间，约 50-100 MB）
#    - 镜像名: 由 $ImageName 和 $ImageTag 配置决定
#
# 2. 创建目录（如不存在）
#    - 影响: 创建认证目录和日志目录
#    - 位置: 由 $AuthDir 和 $LogDir 配置决定
#
# 3. 停止并删除旧容器（如存在同名容器）
#    - 影响: 停止并删除名为 $ContainerName 的现有容器
#
# 4. 运行新容器
#    - 影响: 启动新容器，占用端口 $HostPort
#    - 容器名: 由 $ContainerName 配置决定
#

<#
使用说明
1. 保存脚本到任意位置
2. 修改脚本开头的配置变量（尤其是配置文件路径）
3. 在任意位置运行脚本即可

配置变量说明：
$ImageName      - 镜像名称（默认 yuan019/cli-proxy-api）
$ImageTag       - 镜像标签（默认 latest）
$ContainerName  - 容器名称（默认 cli-proxy-api）
$HostPort       - 主机端口（默认 8317）
$ContainerPort  - 容器端口（默认 8317）
$ConfigFile     - 配置文件路径（必须设置为实际路径）
$AuthDir        - 认证目录路径
$LogDir         - 日志目录路径
$AutoRestart    - 是否自动重启（默认 always）
$PullAlways     - 每次运行是否拉取最新镜像（默认 $true）
#>

$ErrorActionPreference = "Stop"

# ========================================
# 可配置变量（根据需要修改）
# ========================================

# Docker 镜像配置
$ImageName = "yuan019/cli-proxy-api"           # 镜像名称（包含仓库前缀）
$ImageTag = "latest"                           # 镜像标签
$PullAlways = $true                            # 是否每次都拉取最新镜像

# 容器配置
$ContainerName = "cli-proxy-api"               # 容器名称
$HostPort = 8317                               # 主机端口
$ContainerPort = 8317                          # 容器端口
$AutoRestart = "always"                        # 重启策略: no, always, unless-stopped, on-failure

# 系统用户名（修改为你的 Windows 用户名）
$SystemUser = "myuan"

# 路径配置（请修改为实际路径）
# 配置文件路径（绝对路径）
$ConfigFile = "C:\Custom Program Files\CLIProxyAPI-dockeruse\config.yaml"
# 认证目录（与 config.yaml 中的 auth-dir 对应）
$AuthDir = "C:\Users\$SystemUser\.cli-proxy-api"
# 日志目录
$LogDir = "C:\Users\$SystemUser\Documents\workspace\CLIProxyAPI\logs"

# ========================================
# 完整镜像名称
# ========================================
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

function Write-ErrorMsg {
    param([string]$Message)
    Write-Host "✗ $Message" -ForegroundColor Red
}

# ========================================
# 主流程
# ========================================

Write-Host ""
Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Magenta
Write-Host "║     Docker 拉取并运行脚本（使用本地配置）                  ║" -ForegroundColor Magenta
Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Magenta
Write-Host ""
Write-Host "配置信息:" -ForegroundColor Gray
Write-FilePath "镜像名称:" $FullImageName
Write-FilePath "容器名称:" $ContainerName
Write-Info "端口映射:" "${HostPort}:${ContainerPort}"
Write-Info "重启策略:" $AutoRestart
Write-FilePath "配置文件:" $ConfigFile
Write-FilePath "认证目录:" $AuthDir
Write-FilePath "日志目录:" $LogDir
Write-Host ""

try {
    # ========================================
    # 步骤 1: 验证配置文件
    # ========================================
    Write-Step "1/5" "验证配置文件"
    
    if (-not (Test-Path $ConfigFile)) {
        Write-ErrorMsg "配置文件不存在: $ConfigFile"
        Write-Host "请修改脚本开头的 `$ConfigFile 变量为实际配置文件路径" -ForegroundColor Yellow
        throw "配置文件不存在"
    }
    
    Write-FilePath "配置文件:" $ConfigFile "(存在)"
    Write-Success "配置文件验证通过"

    # ========================================
    # 步骤 2: 创建必要目录
    # ========================================
    Write-Step "2/5" "创建必要目录"
    
    # 创建认证目录
    if (-not (Test-Path $AuthDir)) {
        Write-Command "New-Item -ItemType Directory -Path `"$AuthDir`" -Force" "创建认证目录（用于存储认证文件）"
        New-Item -ItemType Directory -Path $AuthDir -Force | Out-Null
        Write-Success "认证目录已创建: $AuthDir"
    } else {
        Write-FilePath "认证目录:" $AuthDir "(已存在，跳过创建)"
    }
    
    # 创建日志目录
    if (-not (Test-Path $LogDir)) {
        Write-Command "New-Item -ItemType Directory -Path `"$LogDir`" -Force" "创建日志目录（用于存储运行日志）"
        New-Item -ItemType Directory -Path $LogDir -Force | Out-Null
        Write-Success "日志目录已创建: $LogDir"
    } else {
        Write-FilePath "日志目录:" $LogDir "(已存在，跳过创建)"
    }

    # ========================================
    # 步骤 3: 拉取 Docker 镜像
    # ========================================
    Write-Step "3/5" "拉取 Docker 镜像"
    
    if ($PullAlways) {
        Write-Command "docker pull $FullImageName" "从 Docker Hub 下载镜像（约 50-100 MB，视网络速度而定）"
        docker pull $FullImageName
        
        if ($LASTEXITCODE -ne 0) {
            throw "Docker 镜像拉取失败"
        }
        Write-Success "镜像拉取成功: $FullImageName"
    } else {
        # 检查镜像是否存在
        $imageExists = docker images -q $FullImageName 2>$null
        if (-not $imageExists) {
            Write-Command "docker pull $FullImageName" "本地镜像不存在，从 Docker Hub 下载（约 50-100 MB）"
            docker pull $FullImageName
            
            if ($LASTEXITCODE -ne 0) {
                throw "Docker 镜像拉取失败"
            }
            Write-Success "镜像拉取成功: $FullImageName"
        } else {
            Write-Info "镜像状态:" "本地已存在（跳过拉取，设置 `$PullAlways=`$true 强制更新）"
        }
    }

    # ========================================
    # 步骤 4: 停止并删除旧容器
    # ========================================
    Write-Step "4/5" "清理旧容器"
    
    # 检查是否存在同名容器
    $existingContainer = docker ps -aq -f "name=^${ContainerName}$" 2>$null
    if ($existingContainer) {
        # 检查容器是否正在运行
        $runningContainer = docker ps -q -f "name=^${ContainerName}$" 2>$null
        if ($runningContainer) {
            Write-Command "docker stop $ContainerName" "停止正在运行的旧容器"
            docker stop $ContainerName | Out-Null
            Write-Success "旧容器已停止"
        }
        
        Write-Command "docker rm $ContainerName" "删除旧容器（释放容器名称）"
        docker rm $ContainerName | Out-Null
        Write-Success "旧容器已删除"
    } else {
        Write-Info "容器状态:" "无同名容器存在（跳过清理）"
    }

    # ========================================
    # 步骤 5: 运行新容器
    # ========================================
    Write-Step "5/5" "运行新容器"
    
    # 构建 docker run 命令
    $runCmd = "docker run -d --name $ContainerName --restart $AutoRestart -p ${HostPort}:${ContainerPort} -v `"${ConfigFile}:/CLIProxyAPI/config.yaml`" -v `"${AuthDir}:/root/.cli-proxy-api`" -v `"${LogDir}:/CLIProxyAPI/logs`" $FullImageName"
    
    Write-Command $runCmd "启动新容器（端口 ${HostPort}，自动重启策略: $AutoRestart）"
    Write-Host ""
    Write-Host "  挂载详情:" -ForegroundColor Gray
    Write-FilePath "    配置文件:" "$ConfigFile -> /CLIProxyAPI/config.yaml"
    Write-FilePath "    认证目录:" "$AuthDir -> /root/.cli-proxy-api"
    Write-FilePath "    日志目录:" "$LogDir -> /CLIProxyAPI/logs"
    Write-Host ""
    
    docker run -d `
        --name $ContainerName `
        --restart $AutoRestart `
        -p "${HostPort}:${ContainerPort}" `
        -v "${ConfigFile}:/CLIProxyAPI/config.yaml" `
        -v "${AuthDir}:/root/.cli-proxy-api" `
        -v "${LogDir}:/CLIProxyAPI/logs" `
        $FullImageName
    
    if ($LASTEXITCODE -ne 0) {
        throw "容器启动失败"
    }
    
    Write-Success "容器启动成功"
    
    # 等待几秒让容器启动
    Write-Host ""
    Write-Host "等待容器启动..." -ForegroundColor Yellow
    Start-Sleep -Seconds 3
    
    # 检查容器状态
    $containerStatus = docker inspect -f '{{.State.Running}}' $ContainerName 2>$null
    if ($containerStatus -eq "true") {
        Write-Success "容器运行正常"
    } else {
        Write-Warning "容器可能未正常启动，请检查日志"
        Write-Command "docker logs $ContainerName" "查看容器日志"
        docker logs $ContainerName 2>&1 | Select-Object -Last 20
    }

    # ========================================
    # 完成
    # ========================================
    Write-Host ""
    Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Green
    Write-Host "║  ✓ 容器已成功启动！                                        ║" -ForegroundColor Green
    Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Green
    Write-Host ""
    Write-Host "服务信息:" -ForegroundColor Cyan
    Write-Info "访问地址:" "http://localhost:$HostPort"
    Write-Info "管理面板:" "http://localhost:$HostPort/management.html"
    Write-Info "请求详情:" "http://localhost:$HostPort/management.html#/detailed-requests"
    Write-Info "API 端点:" "http://localhost:$HostPort/v1/models"
    Write-Host ""
    Write-Host "常用命令:" -ForegroundColor Cyan
    Write-Host "  查看日志:   " -NoNewline -ForegroundColor Gray
    Write-Host "docker logs -f $ContainerName" -ForegroundColor Cyan
    Write-Host "  进入容器:   " -NoNewline -ForegroundColor Gray
    Write-Host "docker exec -it $ContainerName sh" -ForegroundColor Cyan
    Write-Host "  停止容器:   " -NoNewline -ForegroundColor Gray
    Write-Host "docker stop $ContainerName" -ForegroundColor Cyan
    Write-Host "  重启容器:   " -NoNewline -ForegroundColor Gray
    Write-Host "docker restart $ContainerName" -ForegroundColor Cyan
    Write-Host "  删除容器:   " -NoNewline -ForegroundColor Gray
    Write-Host "docker rm -f $ContainerName" -ForegroundColor Cyan
    Write-Host ""

} catch {
    Write-Host ""
    Write-Host "╔════════════════════════════════════════════════════════════╗" -ForegroundColor Red
    Write-Host "║  ✗ 发生错误                                                ║" -ForegroundColor Red
    Write-Host "╚════════════════════════════════════════════════════════════╝" -ForegroundColor Red
    Write-Host ""
    Write-Host "错误信息: $($_.Exception.Message)" -ForegroundColor Red
    Write-Host ""
    exit 1
}