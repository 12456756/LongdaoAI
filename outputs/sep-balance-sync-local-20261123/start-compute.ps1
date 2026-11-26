$ErrorActionPreference = 'Stop'
$env:DATA_DIR = Join-Path $env:TEMP 'longdao-pricing-validation-20261120'
$seedFile = Join-Path $env:TEMP 'longdao-sep-balance-signing-20261123/event-signing-seed.txt'
$env:SEP_EVENT_SIGNING_PRIVATE_KEY = (Get-Content -LiteralPath $seedFile -Raw).Trim()
$env:SEP_EVENT_SIGNING_KEY_ID = 'compute-events-local-20261123'
Set-Location -LiteralPath 'D:/github/LongdaoAI-worktree-one/backend'
& 'D:/github/LongdaoAI-worktree-one/outputs/longdao-sep-password-preview-20261124.exe'
