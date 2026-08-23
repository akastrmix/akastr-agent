$ErrorActionPreference = 'Stop'

function Invoke-GoCommand {
  param([Parameter(Mandatory = $true)][string[]]$Arguments)
  & go @Arguments
  if ($LASTEXITCODE -ne 0) {
    throw "Go verification failed: go $($Arguments -join ' ')"
  }
}

# Exercise cancellation-sensitive maintenance coordination repeatedly before the
# ordinary full suite. Explicit timeouts identify leaked goroutines promptly.
Invoke-GoCommand @('test', '-count=100', '-timeout=2m', './internal/autoupdate')
Invoke-GoCommand @('test', '-count=20', '-timeout=2m', './internal/features/ipwatch')
Invoke-GoCommand @('test', '-count=1', '-timeout=2m', './...')
Invoke-GoCommand @('vet', './...')
Invoke-GoCommand @('build', './cmd/akastr-agent')
