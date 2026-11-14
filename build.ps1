# 编译两端程序到 dist 目录（需要 Go 和 rcodesign，Mac 端在 Windows 上交叉编译并打包成 .app）
param([switch]$Installer)
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force dist | Out-Null
$version = '1.3.11'

go build -tags nodynamic -trimpath -ldflags "-H=windowsgui -s -w" -o dist\kvmswitch.exe ./cmd/kvmswitch-win
if ($LASTEXITCODE -ne 0) { throw "Windows 端编译失败" }

$env:GOOS = 'darwin'; $env:GOARCH = 'arm64'; $env:CGO_ENABLED = '0'
try {
    go build -tags nodynamic -trimpath -ldflags "-s -w" -o dist\kvmswitch-mac ./cmd/kvmswitch-mac
    if ($LASTEXITCODE -ne 0) { throw "Mac 端编译失败" }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
}

go run ./tools/pack dist\kvmswitch-mac dist\KVMSwitch-mac.zip $version
if ($LASTEXITCODE -ne 0) { throw "Mac 应用打包失败" }
Remove-Item dist\kvmswitch-mac

if ($Installer) { & "$PSScriptRoot/packaging/build-msi.ps1" -Version $version }
Get-ChildItem dist | Select-Object Name, Length
