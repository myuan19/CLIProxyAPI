# dev-start.ps1 - 开发环境一键启动脚本
# 功能：构建前端 -> 复制到后端 -> 启动后端服务
#
# ========================================
# 脚本影响说明
# ========================================
# 本脚本会执行以下操作，可能影响你的文件系统：
# 1. 在前端目录执行 npm install
#    - 影响: 创建/更新 node_modules 目录（如果不存在）
#    - 位置: Cli-Proxy-API-Management-Center/node_modules
#
# 2. 在前端目录执行 npm run build
#    - 影响: 创建/覆盖 dist/index.html 文件（单文件 HTML，包含所有前端代码）
#    - 位置: Cli-Proxy-API-Management-Center/dist/index.html
#
# 3. 创建 static 目录（如果不存在）
#    - 影响: 创建新目录
#    - 位置: CLIProxyAPI/static
#
# 4. 复制前端构建文件
#    - 影响: 复制并覆盖 CLIProxyAPI/static/management.html（如果已存在会被覆盖）
#    - 源文件: Cli-Proxy-API-Management-Center/dist/index.html
#    - 目标文件: CLIProxyAPI/static/management.html
#
# 5. 创建配置文件（如果不存在）
#    - 影响: 从 config.example.yaml 复制创建 config.yaml（如果已存在不会被覆盖）
#    - 位置: CLIProxyAPI/config.yaml
#
# 6. 设置环境变量
#    - 影响: 设置 MANAGEMENT_STATIC_PATH 环境变量（仅当前 PowerShell 会话有效）
#
# 7. 启动 Go 后端服务
#    - 影响: 运行 go run cmd/server/main.go，启动 HTTP 服务器
#    - 端口: 默认 8317（可在 config.yaml 中配置）

$ErrorActionPreference = "Stop"

# ========================================
# 可配置变量（可根据实际情况修改）
# ========================================
$FrontendProjectName = "Cli-Proxy-API-Management-Center"  # 前端项目目录名
$BackendProjectName = "CLIProxyAPI"                      # 后端项目目录名
$StaticDirName = "static"                                 # 静态文件目录名
$ManagementHtmlFileName = "management.html"               # 管理界面文件名
$ConfigFileName = "config.yaml"                           # 配置文件名称
$ConfigExampleFileName = "config.example.yaml"            # 配置文件示例名称
$BackendPort = 8317                                       # 后端服务端口（仅用于显示，实际从 config.yaml 读取）

# ========================================
# 计算路径（基于脚本位置）
# ========================================
# 获取脚本所在目录（假设脚本在 CLIProxyAPI/_scripts/ 目录下）
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BackendDir = Split-Path -Parent (Split-Path -Parent $ScriptDir)  # CLIProxyAPI 目录（从 .dev/scripts/ 往上两层）
$WorkspaceRoot = Split-Path -Parent $BackendDir  # 工作区根目录

$FrontendDir = Join-Path $WorkspaceRoot $FrontendProjectName
$BackendStaticDir = Join-Path $BackendDir $StaticDirName
$ManagementHtmlPath = Join-Path $BackendStaticDir $ManagementHtmlFileName
$ConfigFile = Join-Path $BackendDir $ConfigFileName
$ConfigExampleFile = Join-Path $BackendDir $ConfigExampleFileName
$DistIndexHtml = Join-Path $FrontendDir "dist" "index.html"

# ========================================
# 辅助函数：打印命令（蓝色）
# ========================================
function Write-Command {
    param(
        [string]$Command,
        [string]$Description = ""
    )
    Write-Host "> " -NoNewline -ForegroundColor Blue
    Write-Host $Command -ForegroundColor Cyan
    if ($Description) {
        Write-Host "  " -NoNewline
        Write-Host "影响: $Description" -ForegroundColor DarkGray
    }
}

# ========================================
# 主流程
# ========================================
Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  开发环境一键启动脚本" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "配置信息:" -ForegroundColor Gray
Write-Host "  工作区根目录: $WorkspaceRoot" -ForegroundColor Gray
Write-Host "  前端项目目录: $FrontendDir" -ForegroundColor Gray
Write-Host "  后端项目目录: $BackendDir" -ForegroundColor Gray
Write-Host "  静态文件目录: $BackendStaticDir" -ForegroundColor Gray
Write-Host ""

# 步骤 1: 检查前端目录
Write-Host "[1/5] 检查前端项目..." -ForegroundColor Yellow
if (-not (Test-Path $FrontendDir)) {
    Write-Host "错误: 找不到前端项目目录: $FrontendDir" -ForegroundColor Red
    Write-Host "提示: 请检查 FrontendProjectName 配置是否正确（当前值: $FrontendProjectName）" -ForegroundColor Yellow
    exit 1
}
Write-Host "✓ 前端项目目录存在" -ForegroundColor Green

# 步骤 2: 检查并安装前端依赖
Write-Host "[2/5] 检查前端依赖..." -ForegroundColor Yellow
Push-Location $FrontendDir
try {
    if (-not (Test-Path "node_modules")) {
        Write-Command "npm install" "在 $FrontendDir 目录安装 npm 依赖包，创建 node_modules 目录（可能需要几分钟）"
        npm install
        if ($LASTEXITCODE -ne 0) {
            Write-Host "错误: 前端依赖安装失败" -ForegroundColor Red
            exit 1
        }
        Write-Host "✓ 前端依赖安装完成" -ForegroundColor Green
    } else {
        Write-Host "✓ 前端依赖已存在 (node_modules 目录已存在，跳过安装)" -ForegroundColor Green
    }
} finally {
    Pop-Location
}

# 步骤 3: 构建前端
Write-Host "[3/5] 构建前端项目..." -ForegroundColor Yellow
Push-Location $FrontendDir
try {
    Write-Command "npm run build" "在 $FrontendDir 目录执行构建，生成 dist/index.html 文件（单文件 HTML，包含所有前端代码，约 1-5 MB）"
    npm run build
    if ($LASTEXITCODE -ne 0) {
        Write-Host "错误: 前端构建失败" -ForegroundColor Red
        exit 1
    }
    
    # 检查构建输出
    if (-not (Test-Path $DistIndexHtml)) {
        Write-Host "错误: 构建输出文件不存在: $DistIndexHtml" -ForegroundColor Red
        exit 1
    }
    $fileSize = (Get-Item $DistIndexHtml).Length
    Write-Host "✓ 前端构建成功 (文件大小: $([math]::Round($fileSize/1MB, 2)) MB)" -ForegroundColor Green
} finally {
    Pop-Location
}

# 步骤 4: 复制文件到后端目录
Write-Host "[4/5] 复制前端文件到后端..." -ForegroundColor Yellow
try {
    # 创建 static 目录（如果不存在）
    if (-not (Test-Path $BackendStaticDir)) {
        Write-Command "New-Item -ItemType Directory -Path `"$BackendStaticDir`"" "创建目录: $BackendStaticDir"
        New-Item -ItemType Directory -Path $BackendStaticDir -Force | Out-Null
        Write-Host "✓ 已创建 static 目录" -ForegroundColor Green
    } else {
        Write-Host "✓ static 目录已存在" -ForegroundColor Green
    }
    
    # 复制文件
    $overwriteWarning = ""
    if (Test-Path $ManagementHtmlPath) {
        $overwriteWarning = "（将覆盖已存在的文件）"
    }
    Write-Command "Copy-Item -Path `"$DistIndexHtml`" -Destination `"$ManagementHtmlPath`" -Force" "复制文件: $DistIndexHtml -> $ManagementHtmlPath $overwriteWarning"
    Copy-Item -Path $DistIndexHtml -Destination $ManagementHtmlPath -Force
    $copiedSize = (Get-Item $ManagementHtmlPath).Length
    Write-Host "✓ 文件已复制到: $ManagementHtmlPath" -ForegroundColor Green
    Write-Host "  文件大小: $([math]::Round($copiedSize/1MB, 2)) MB" -ForegroundColor Gray
    Write-Host "  最后修改时间: $((Get-Item $ManagementHtmlPath).LastWriteTime)" -ForegroundColor Gray
} catch {
    Write-Host "错误: 复制文件失败: $_" -ForegroundColor Red
    exit 1
}

# 步骤 5: 设置环境变量并启动后端
Write-Host "[5/5] 启动后端服务..." -ForegroundColor Yellow
Write-Host ""

# 检查配置文件
if (-not (Test-Path $ConfigFile)) {
    if (Test-Path $ConfigExampleFile) {
        Write-Command "Copy-Item -Path `"$ConfigExampleFile`" -Destination `"$ConfigFile`" -Force" "创建配置文件: 从 $ConfigExampleFile 复制到 $ConfigFile（如果 config.yaml 已存在则不会覆盖）"
        Copy-Item -Path $ConfigExampleFile -Destination $ConfigFile -Force
        Write-Host "✓ 已创建 config.yaml（从 config.example.yaml 复制）" -ForegroundColor Green
        Write-Host "  提示: 请根据需要修改 config.yaml 中的配置（如管理密钥、端口等）" -ForegroundColor Yellow
    } else {
        Write-Host "警告: 未找到 config.yaml 和 config.example.yaml，将使用默认配置" -ForegroundColor Yellow
    }
} else {
    Write-Host "✓ 配置文件已存在: $ConfigFile" -ForegroundColor Green
}

# 设置环境变量
Write-Command "`$env:MANAGEMENT_STATIC_PATH = `"$ManagementHtmlPath`"" "设置环境变量 MANAGEMENT_STATIC_PATH（仅当前 PowerShell 会话有效，用于指定本地前端文件路径，避免从 GitHub 下载）"
$env:MANAGEMENT_STATIC_PATH = $ManagementHtmlPath

# ========================================
# 验证环境变量设置
# ========================================
Write-Host ""
Write-Host "验证环境变量设置:" -ForegroundColor Yellow
if ($env:MANAGEMENT_STATIC_PATH -eq $ManagementHtmlPath) {
    Write-Host "✓ MANAGEMENT_STATIC_PATH 已正确设置" -ForegroundColor Green
    Write-Host "  值: $env:MANAGEMENT_STATIC_PATH" -ForegroundColor Gray
    Write-Host "  后端将使用本地文件，不会从 GitHub 下载" -ForegroundColor Green
} else {
    Write-Host "✗ 警告: MANAGEMENT_STATIC_PATH 设置失败！" -ForegroundColor Red
    Write-Host "  期望值: $ManagementHtmlPath" -ForegroundColor Red
    Write-Host "  实际值: $env:MANAGEMENT_STATIC_PATH" -ForegroundColor Red
    Write-Host "  后端可能会从 GitHub 下载文件，覆盖本地修改！" -ForegroundColor Red
}

# 记录本地文件的 hash，用于启动后验证
$script:LocalFileHash = (Get-FileHash $ManagementHtmlPath).Hash
Write-Host "  本地文件 Hash: $($script:LocalFileHash.Substring(0,16))..." -ForegroundColor Gray

Write-Host ""
Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  启动后端服务..." -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "前端管理界面: http://localhost:$BackendPort/management.html" -ForegroundColor Green
Write-Host "按 Ctrl+C 停止服务" -ForegroundColor Yellow
Write-Host ""
Write-Host "提示: 如果健康检查按钮没有出现，请检查:" -ForegroundColor Yellow
Write-Host "  1. 浏览器是否强制刷新 (Ctrl+F5)" -ForegroundColor Gray
Write-Host "  2. 运行以下命令验证文件是否被覆盖:" -ForegroundColor Gray
Write-Host "     (Get-FileHash `"$ManagementHtmlPath`").Hash -eq `"$($script:LocalFileHash)`"" -ForegroundColor Cyan
Write-Host "     Hash 值应与启动时的记录一致" -ForegroundColor Gray
Write-Host ""

# 切换到后端目录并启动
Push-Location $BackendDir
try {
    Write-Command "go run cmd/server/main.go" "启动 Go 后端服务（在 $BackendDir 目录执行，会读取 config.yaml 配置，监听端口 $BackendPort）"
    go run cmd/server/main.go
} finally {
    Pop-Location
}