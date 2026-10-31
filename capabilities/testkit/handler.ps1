# testkit capability handler, hanse.handler/v1.
#
# The operations only write output, wait, or fail on request. Read the whole
# request from stdin, write one response to stdout, and use stderr for log
# lines only. handler.sh is the Linux and macOS handler.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try {
    [Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
} catch {
    # A host without a console keeps its own output encoding.
}
$OutputEncoding = [System.Text.UTF8Encoding]::new($false)

$contract = 'hanse.handler/v1'

function Write-Log([string]$Line) {
    [Console]::Error.WriteLine($Line)
    [Console]::Error.Flush()
}

function Write-StandardOutput([string]$Text) {
    # UTF-8 without a byte order mark, straight to the standard output stream.
    $stream = [Console]::OpenStandardOutput()
    $bytes = [System.Text.UTF8Encoding]::new($false).GetBytes($Text)
    $stream.Write($bytes, 0, $bytes.Length)
    $stream.Flush()
}

function Write-Response($Response, [int]$Code) {
    $json = $Response | ConvertTo-Json -Depth 20 -Compress
    Write-StandardOutput ($json + "`n")
    exit $Code
}

function Reply-Ok($Result, [string]$Summary) {
    $response = [ordered]@{ contract = $contract; status = 'ok'; result = $Result }
    if ($Summary) { $response.summary = $Summary }
    Write-Response $response 0
}

function Reply-Cancelled($Result, [string]$Summary) {
    $response = [ordered]@{ contract = $contract; status = 'cancelled'; result = $Result }
    if ($Summary) { $response.summary = $Summary }
    Write-Response $response 0
}

function Reply-Error([string]$Code, [string]$Message, $Details) {
    $errorObject = [ordered]@{ code = $Code; message = $Message }
    if ($null -ne $Details) { $errorObject.details = $Details }
    Write-Response ([ordered]@{ contract = $contract; status = 'error'; error = $errorObject }) 1
}

# A console host turns redirected stdin into the script's pipeline input.
# Opening the stdin handle as a second reader deadlocks on Windows, because
# the host keeps the pipe open, so the pipeline is the only reader. The
# manifest's -InputFormat Text keeps every stdin line a string.
$raw = @($input) -join "`n"

try {
    $req = $raw | ConvertFrom-Json
} catch {
    Reply-Error 'invalid_arguments' 'the request body is not valid JSON'
}

function Get-Argument([string]$Name) {
    if ($null -eq $req.arguments) { return $null }
    $property = $req.arguments.PSObject.Properties[$Name]
    if ($null -eq $property) { return $null }
    return $property.Value
}

function Get-IntArgument([string]$Name, [int]$Default) {
    $value = Get-Argument $Name
    if ($null -eq $value) { return $Default }
    try { return [int]$value } catch { return $Default }
}

function Test-StopRequested {
    if (-not $env:HANSE_CANCEL_FILE) { return $false }
    return Test-Path -LiteralPath $env:HANSE_CANCEL_FILE
}

function Write-FloodLogs([int]$Bytes) {
    $line = 'x' * 1023
    $written = 0
    while ($written -lt $Bytes) {
        Write-Log "flood: $line"
        $written += 1029
    }
    Write-Log "flood: wrote $written bytes of logs"
}

if ($req.kind -eq 'options') {
    $operation = $req.operation
    $optionField = $req.field
    if ($operation -ne 'choose' -or $optionField -ne 'choice') {
        Reply-Error 'unknown_field' "no options for $operation.$optionField"
    }
    Reply-Ok ([ordered]@{
            options = @(
                [ordered]@{ value = 'alpha'; label = 'Alpha' },
                [ordered]@{ value = 'beta'; label = 'Beta'; description = 'The second value' },
                [ordered]@{ value = 'gamma'; label = 'Gamma' }
            )
        }) ''
}

switch ($req.operation) {
    'say' {
        # The target node is optional: it proves that the envelope holds a
        # node an argument refers to, and the result reports it back.
        $targetId = Get-Argument 'target'
        $target = $null
        if ($targetId) {
            $property = $req.nodes.PSObject.Properties[$targetId]
            if ($null -eq $property) {
                Reply-Error 'unknown_node' 'the target argument names no node of this request'
            }
            $target = $property.Value
        }
        $text = Get-Argument 'text'
        if (-not $text) { $text = '(no text)' }
        Write-Log "say: $text"
        $executor = [ordered]@{ id = $req.executor.id; os = $req.executor.os; arch = $req.executor.arch }
        Reply-Ok ([ordered]@{ arguments = $req.arguments; executor = $executor; target = $target }) 'said the arguments back'
    }

    'long' {
        $seconds = Get-IntArgument 'seconds' 5
        Write-Log "long: running for $seconds seconds"
        for ($i = 1; $i -le $seconds; $i++) {
            if (Test-StopRequested) {
                Write-Log "long: stop requested at $($i - 1) of $seconds"
                Reply-Cancelled ([ordered]@{ seconds = $i - 1 }) "stopped after $($i - 1) seconds"
            }
            Write-Log "long: second $i of $seconds"
            Start-Sleep -Seconds 1
        }
        Reply-Ok ([ordered]@{ seconds = $seconds }) "ran for $seconds seconds"
    }

    'background' {
        $seconds = Get-IntArgument 'seconds' 300
        $interval = Get-IntArgument 'interval_seconds' 60
        Write-Log "background: running for $seconds seconds, one line every $interval seconds"
        $elapsed = 0
        $tick = 0
        while ($elapsed -lt $seconds) {
            if (Test-StopRequested) {
                Write-Log "background: stop requested after $elapsed of $seconds seconds"
                Reply-Cancelled ([ordered]@{ seconds = $elapsed; ticks = $tick }) "stopped after $elapsed seconds"
            }
            if (($elapsed % $interval) -eq 0) {
                $tick++
                Write-Log "background: tick $tick after $elapsed of $seconds seconds"
            }
            Start-Sleep -Seconds 1
            $elapsed++
        }
        Reply-Ok ([ordered]@{ seconds = $seconds; ticks = $tick }) "ran for $seconds seconds"
    }

    'fail' {
        $errorCode = Get-Argument 'error_code'
        if ($errorCode) {
            $message = Get-Argument 'message'
            if (-not $message) { $message = 'the testkit handler failed on request' }
            $details = Get-Argument 'details'
            Write-Log "fail: handler error $errorCode"
            Reply-Error $errorCode $message $details
        }
        $exitCode = Get-IntArgument 'exit_code' 1
        Write-Log "fail: exiting with code $exitCode"
        $body = [ordered]@{ contract = $contract; status = 'ok'; result = [ordered]@{ exit_code = $exitCode } }
        Write-StandardOutput (($body | ConvertTo-Json -Depth 20 -Compress) + "`n")
        exit $exitCode
    }

    'hang' {
        $seconds = Get-IntArgument 'seconds' 600
        # Log before the loop ignores the stop request, so a test can see the
        # handler running before it cancels. Windows has no SIGTERM, so the
        # cancel file is simply never read.
        Write-Log "hang: ignoring stop requests for $seconds seconds"
        $elapsed = 0
        while ($elapsed -lt $seconds) {
            Start-Sleep -Seconds 1
            $elapsed++
            if (($elapsed % 10) -eq 0) {
                Write-Log "hang: still running after $elapsed seconds"
            }
        }
        Reply-Ok ([ordered]@{ seconds = $seconds }) "hung for $seconds seconds"
    }

    'timeout' {
        $seconds = Get-IntArgument 'seconds' 30
        # Ignore the stop request: a cancelled response would end the run as
        # cancelled, and this operation tests the manifest deadline.
        Write-Log "timeout: running for $seconds seconds, beyond the manifest timeout"
        $elapsed = 0
        while ($elapsed -lt $seconds) {
            Start-Sleep -Seconds 1
            $elapsed++
            if (($elapsed % 5) -eq 0) {
                Write-Log "timeout: still running after $elapsed seconds"
            }
        }
        Reply-Ok ([ordered]@{ seconds = $seconds }) "finished after $seconds seconds"
    }

    'flood' {
        $mode = Get-Argument 'mode'
        if (-not $mode) { $mode = 'both' }
        $kilobytes = Get-IntArgument 'kilobytes' 12288
        $bytes = $kilobytes * 1024
        if ($mode -eq 'stderr') {
            Write-FloodLogs $bytes
            Reply-Ok ([ordered]@{ logs_bytes = $bytes }) "wrote $bytes bytes of logs"
        } elseif ($mode -eq 'stdout' -or $mode -eq 'both') {
            if ($mode -eq 'both') { Write-FloodLogs $bytes }
            # One response larger than the response limit; the padding is
            # only 'x', so no JSON escaping is needed.
            $padding = 'x' * $bytes
            Write-StandardOutput ('{"contract":"' + $contract + '","status":"ok","result":{"padding":"' + $padding + '"}}' + "`n")
            exit 0
        } else {
            Reply-Error 'invalid_arguments' 'mode must be stdout, stderr, or both'
        }
    }

    'choose' {
        $choice = Get-Argument 'choice'
        $known = $choice -eq 'alpha' -or $choice -eq 'beta' -or $choice -eq 'gamma'
        Write-Log "choose: $choice"
        Reply-Ok ([ordered]@{ choice = $choice; known = $known }) "chose $choice"
    }

    'privileged' {
        $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
        $principal = [Security.Principal.WindowsPrincipal]::new($identity)
        $admin = $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
        $envValue = $env:HANSE_PRIVILEGED
        if (-not $envValue) { $envValue = '0' }
        $user = $identity.Name
        Write-Log "privileged: HANSE_PRIVILEGED=$envValue admin=$admin user=$user"
        Reply-Ok ([ordered]@{
                hanse_privileged = $envValue
                privileged       = ($envValue -eq '1')
                admin            = $admin
                user             = $user
                os               = 'windows'
            }) "privileged=$envValue admin=$admin"
    }

    default {
        Reply-Error 'unknown_operation' "unknown operation: $($req.operation)"
    }
}
