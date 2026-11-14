param([string]$Version='1.3.5',[string]$WixBin=$env:WIX_BIN)
$ErrorActionPreference='Stop'
Push-Location (Split-Path $PSScriptRoot)
try {
 if (!$WixBin) { $WixBin=Split-Path (Get-Command candle.exe -ErrorAction Stop).Source }
 $obj=Join-Path $env:TEMP ("kvm-"+[guid]::NewGuid()+".wixobj")
 & (Join-Path $WixBin 'candle.exe') -nologo -arch x64 "-dVersion=$Version" -out $obj packaging/windows.wxs
 if($LASTEXITCODE){throw "candle failed"}
 & (Join-Path $WixBin 'light.exe') -nologo -spdb -out dist/KVMSwitch-Setup.msi $obj
 if($LASTEXITCODE){throw "light failed"}
} finally { if($obj -and (Test-Path -LiteralPath $obj)){Remove-Item -LiteralPath $obj}; Pop-Location }
